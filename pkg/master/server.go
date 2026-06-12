package master

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"gopass/pkg/db"
	pb "gopass/pkg/proto"
	"gopass/pkg/types"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ActiveAgent stores the active stream connection and last seen time for an Agent
type ActiveAgent struct {
	Alias    string
	Stream   pb.AgentService_TunnelServer
	LastSeen time.Time
	IP       string
}

// Server wraps the gRPC server and database manager
type Server struct {
	pb.UnimplementedAgentServiceServer
	dbManager   *db.Manager
	token       string
	addr        string
	mu          sync.RWMutex
	tlsEnabled  bool
	tlsCertPath string
	tlsKeyPath  string
	agents      map[string]*ActiveAgent
	grpcServer  *grpc.Server
	stopChan    chan struct{}
	onProgress  func(*pb.TaskProgress)
	onHeartbeat func(string, float64, float64)
}

// NewServer initializes and returns a Server instance
func NewServer(dbManager *db.Manager, addr string, tlsEnabled bool, certPath, keyPath string) (*Server, error) {
	token, err := getOrInitCommunicationToken(dbManager)
	if err != nil {
		return nil, err
	}

	return &Server{
		dbManager:   dbManager,
		token:       token,
		addr:        addr,
		tlsEnabled:  tlsEnabled,
		tlsCertPath: certPath,
		tlsKeyPath:  keyPath,
		agents:      make(map[string]*ActiveAgent),
		stopChan:    make(chan struct{}),
	}, nil
}

// SetProgressCallback registers a listener for Agent build progress updates
func (s *Server) SetProgressCallback(cb func(*pb.TaskProgress)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onProgress = cb
}

// SetHeartbeatCallback registers a listener for Agent heartbeat updates
func (s *Server) SetHeartbeatCallback(cb func(string, float64, float64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onHeartbeat = cb
}



// Start runs the gRPC server and background tasks (like heartbeat checking)
func (s *Server) Start() error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}

	var opts []grpc.ServerOption
	if s.tlsEnabled {
		var creds credentials.TransportCredentials
		if s.tlsCertPath != "" && s.tlsKeyPath != "" {
			var err error
			creds, err = credentials.NewServerTLSFromFile(s.tlsCertPath, s.tlsKeyPath)
			if err != nil {
				return fmt.Errorf("failed to load TLS keys: %w", err)
			}
			log.Printf("[SECURITY] gRPC TLS enabled using certificate file: %s", s.tlsCertPath)
		} else {
			// Generate in-memory self-signed cert
			cert, err := types.GenerateSelfSignedCert()
			if err != nil {
				return fmt.Errorf("failed to generate self-signed TLS cert: %w", err)
			}
			creds = credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{cert},
			})
			log.Println("[SECURITY] gRPC TLS enabled using dynamic in-memory self-signed certificate")
		}
		opts = append(opts, grpc.Creds(creds))
	}

	s.grpcServer = grpc.NewServer(opts...)
	pb.RegisterAgentServiceServer(s.grpcServer, s)

	// Start gRPC server in a goroutine
	go func() {
		log.Printf("[gRPC] Master listening on %s", s.addr)
		if err := s.grpcServer.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Printf("[gRPC] Server error: %v", err)
		}
	}()

	// Start background heartbeat scanner
	go s.heartbeatChecker()

	return nil
}

// Stop shuts down the gRPC server and stops background goroutines
func (s *Server) Stop() {
	close(s.stopChan)
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
}

// Tunnel implements the bidirectional streaming RPC between Agent and Master
func (s *Server) Tunnel(stream pb.AgentService_TunnelServer) error {
	// 1. Authenticate connection (expect first message to be RegisterRequest)
	firstMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive registration message: %w", err)
	}

	regReq := firstMsg.GetRegister()
	if regReq == nil {
		return status.Errorf(codes.InvalidArgument, "first message must be a registration request")
	}

	alias := regReq.GetAlias()
	token := regReq.GetToken()

	if token != s.token {
		maskedToken := token
		if len(token) > 4 {
			maskedToken = token[:4] + "***"
		} else {
			maskedToken = "***"
		}
		log.Printf("[SECURITY] Auth failed for agent alias '%s' with token '%s'", alias, maskedToken)
		_ = stream.Send(&pb.MasterMessage{
			Payload: &pb.MasterMessage_RegisterResponse{
				RegisterResponse: &pb.RegisterResponse{
					Success:      false,
					ErrorMessage: "invalid communication token",
				},
			},
		})
		return status.Errorf(codes.Unauthenticated, "invalid token")
	}

	// 2. Register active stream
	var agentIP string
	if p, ok := peer.FromContext(stream.Context()); ok {
		agentIP, _, _ = net.SplitHostPort(p.Addr.String())
	}
	if agentIP == "" {
		agentIP = "127.0.0.1"
	}

	log.Printf("[gRPC] Agent '%s' registered successfully from IP %s", alias, agentIP)
	s.mu.Lock()
	s.agents[alias] = &ActiveAgent{
		Alias:    alias,
		Stream:   stream,
		LastSeen: time.Now(),
		IP:       agentIP,
	}
	s.mu.Unlock()

	// Update database to mark node as online
	node, err := s.dbManager.GetNode(alias)
	if err != nil {
		node = &types.ServerNode{
			Alias: alias,
		}
		if !errors.Is(err, db.ErrNotFound) {
			log.Printf("[db] Error querying node %s: %v", alias, err)
		}
	}
	node.Connected = true
	node.LastSeen = time.Now()
	node.IP = agentIP
	if err := s.dbManager.SaveNode(node); err != nil {
		log.Printf("[db] Failed to update node online status for %s: %v", alias, err)
	}

	// Send successful registration response
	err = stream.Send(&pb.MasterMessage{
		Payload: &pb.MasterMessage_RegisterResponse{
			RegisterResponse: &pb.RegisterResponse{
				Success: true,
			},
		},
	})
	if err != nil {
		s.handleDisconnect(alias)
		return err
	}

	// 3. Receive messages from Agent (Heartbeats & TaskProgress)
	for {
		in, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			log.Printf("[gRPC] Agent '%s' disconnected (EOF)", alias)
			break
		}
		if err != nil {
			log.Printf("[gRPC] Stream error from Agent '%s': %v", alias, err)
			break
		}

		s.mu.Lock()
		if agent, exists := s.agents[alias]; exists {
			agent.LastSeen = time.Now()
		}
		s.mu.Unlock()

		switch payload := in.Payload.(type) {
		case *pb.AgentMessage_Heartbeat:
			s.handleHeartbeat(payload.Heartbeat)
		case *pb.AgentMessage_Progress:
			s.handleProgress(payload.Progress)
		}
	}

	s.handleDisconnect(alias)
	return nil
}

// Deploy sends a DeployTask down to a specific connected Agent
func (s *Server) Deploy(alias string, task *pb.DeployTask) error {
	s.mu.RLock()
	agent, exists := s.agents[alias]
	s.mu.RUnlock()

	if !exists {
		return fmt.Errorf("agent '%s' is not currently connected", alias)
	}

	return agent.Stream.Send(&pb.MasterMessage{
		Payload: &pb.MasterMessage_DeployTask{
			DeployTask: task,
		},
	})
}

// SendUpdateTask sends an UpdateAgentTask down to a specific connected Agent
func (s *Server) SendUpdateTask(alias string, downloadURL string, githubToken string, tagName string) bool {
	s.mu.RLock()
	agent, exists := s.agents[alias]
	s.mu.RUnlock()

	if !exists {
		return false
	}

	err := agent.Stream.Send(&pb.MasterMessage{
		Payload: &pb.MasterMessage_UpdateAgentTask{
			UpdateAgentTask: &pb.UpdateAgentTask{
				DownloadUrl: downloadURL,
				GithubToken: githubToken,
				TagName:     tagName,
			},
		},
	})
	return err == nil
}

// handleHeartbeat updates node hardware stats in bbolt
func (s *Server) handleHeartbeat(h *pb.Heartbeat) {
	node, err := s.dbManager.GetNode(h.Alias)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			node = &types.ServerNode{Alias: h.Alias}
		} else {
			log.Printf("[db] Error fetching node %s: %v", h.Alias, err)
			return
		}
	}

	node.Connected = true
	node.LastSeen = time.Now()
	node.Hardware.CPUUsage = h.CpuUsage
	node.Hardware.MemoryUsage = h.MemoryUsage
	node.Hardware.DiskUsage = h.DiskUsage

	if err := s.dbManager.SaveNode(node); err != nil {
		log.Printf("[db] Error saving node heartbeat %s: %v", h.Alias, err)
	}

	s.mu.RLock()
	cb := s.onHeartbeat
	s.mu.RUnlock()
	if cb != nil {
		cb(h.Alias, h.CpuUsage, h.MemoryUsage)
	}
}

// handleProgress processes logs from Agent's build task (Phase 3 & 4 logic hooks)
func (s *Server) handleProgress(p *pb.TaskProgress) {
	s.mu.RLock()
	cb := s.onProgress
	s.mu.RUnlock()

	if cb != nil {
		cb(p)
	} else {
		log.Printf("[BUILD-PROGRESS] Task: %s, State: %s, Log: %s", p.TaskId, p.State, p.LogLine)
	}
}

// handleDisconnect removes Agent from memory mapping and updates database
func (s *Server) handleDisconnect(alias string) {
	s.mu.Lock()
	delete(s.agents, alias)
	s.mu.Unlock()

	node, err := s.dbManager.GetNode(alias)
	if err == nil {
		node.Connected = false
		if err := s.dbManager.SaveNode(node); err != nil {
			log.Printf("[db] Failed to set node offline %s: %v", alias, err)
		}
	}
}

// heartbeatChecker scans active agents periodically, flagging inactive ones
func (s *Server) heartbeatChecker() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			var lostAliases []string
			s.mu.Lock()
			now := time.Now()
			for alias, agent := range s.agents {
				if now.Sub(agent.LastSeen) > 30*time.Second {
					log.Printf("[ALERT] Agent '%s' missed heartbeats for 30s. Declaring lost connection.", alias)
					delete(s.agents, alias)
					lostAliases = append(lostAliases, alias)
				}
			}
			s.mu.Unlock()

			if len(lostAliases) > 0 {
				go func(aliases []string) {
					if err := s.dbManager.MarkNodesOffline(aliases); err != nil {
						log.Printf("[db] Failed to update offline status for lost agents: %v", err)
					}
				}(lostAliases)
			}
		case <-s.stopChan:
			return
		}
	}
}

// getOrInitCommunicationToken reads or generates a secure random token
func getOrInitCommunicationToken(dbManager *db.Manager) (string, error) {
	token, err := dbManager.GetCommunicationToken()
	if err == nil && token != "" {
		return token, nil
	}

	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return "", fmt.Errorf("failed to read token: %w", err)
	}

	// Generate 16 bytes random hex (32 characters)
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random token: %w", err)
	}
	token = hex.EncodeToString(bytes)

	if err := dbManager.SetCommunicationToken(token); err != nil {
		return "", fmt.Errorf("failed to save generated token: %w", err)
	}

	log.Printf("\n==================================================\n[SECURITY] Generated Communication Token: %s\nPlease configure this on the Agent nodes.\n==================================================\n", token)
	return token, nil
}
