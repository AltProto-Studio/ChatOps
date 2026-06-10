package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	pb "gopass/pkg/proto"
	"gopass/pkg/types"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client handles gRPC communication from Agent to Master
type Client struct {
	alias      string
	token      string
	masterAddr string
	insecure   bool
	stopChan   chan struct{}
	stopOnce   sync.Once
	activeSend chan *pb.AgentMessage
}

// NewClient creates a new Agent Client instance
func NewClient(alias, token, masterAddr string, insecure bool) *Client {
	return &Client{
		alias:      alias,
		token:      token,
		masterAddr: masterAddr,
		insecure:   insecure,
		stopChan:   make(chan struct{}),
		activeSend: make(chan *pb.AgentMessage, 100),
	}
}

// Start initiates the connection and reconnection loop
func (c *Client) Start() {
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second

	for {
		log.Printf("[Agent] Attempting to connect to Master at %s...", c.masterAddr)
		err := c.connectAndRun()
		if err != nil {
			log.Printf("[Agent] Connection error: %v. Reconnecting in %v...", err, backoff)
			select {
			case <-c.stopChan:
				log.Println("[Agent] Client stopped.")
				return
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		} else {
			// Connection ran successfully and closed cleanly
			backoff = 1 * time.Second
			select {
			case <-c.stopChan:
				log.Println("[Agent] Client stopped.")
				return
			default:
			}
		}
	}
}

// Stop shuts down the client connection
func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})
}

// connectAndRun establishes gRPC connection and runs communication loops
func (c *Client) connectAndRun() error {
	// Set up gRPC dial options
	var opts []grpc.DialOption
	if c.insecure {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		// In production, we'd load TLS credentials here.
		// For verification simplicity in Phase 2, we fallback to insecure.
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.Dial(c.masterAddr, opts...)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer conn.Close()

	client := pb.NewAgentServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle stop signal
	go func() {
		<-c.stopChan
		cancel()
	}()

	stream, err := client.Tunnel(ctx)
	if err != nil {
		return fmt.Errorf("failed to create tunnel: %w", err)
	}

	// 1. Send Registration message
	err = stream.Send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_Register{
			Register: &pb.RegisterRequest{
				Alias: c.alias,
				Token: c.token,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send registration message: %w", err)
	}

	// Receive registration response
	resp, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive registration response: %w", err)
	}

	regResp := resp.GetRegisterResponse()
	if regResp == nil {
		return fmt.Errorf("expected register response, got: %T", resp.Payload)
	}

	if !regResp.Success {
		return fmt.Errorf("registration rejected by Master: %s", regResp.ErrorMessage)
	}

	log.Printf("[Agent] Registered successfully as '%s'", c.alias)

	// Channel to signal internal worker errors
	errChan := make(chan error, 3)

	// 2. Start sender loop
	go func() {
		// Ticker for periodic heartbeats
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-c.activeSend:
				if err := stream.Send(msg); err != nil {
					errChan <- fmt.Errorf("failed to send active message: %w", err)
					return
				}
			case <-ticker.C:
				// Asynchronously fetch stats to avoid blocking the main stream sender loop (e.g. cpu.Percent blocks for 500ms)
				go func() {
					cpuVal, memVal, diskVal := getSystemStats()
					hMsg := &pb.AgentMessage{
						Payload: &pb.AgentMessage_Heartbeat{
							Heartbeat: &pb.Heartbeat{
								Alias:       c.alias,
								CpuUsage:    cpuVal,
								MemoryUsage: memVal,
								DiskUsage:   diskVal,
							},
						},
					}
					select {
					case c.activeSend <- hMsg:
					case <-ctx.Done():
					}
				}()
			}
		}
	}()

	// 3. Start receiver loop (for DeployTask commands from Master)
	go func() {
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				errChan <- io.EOF
				return
			}
			if err != nil {
				errChan <- fmt.Errorf("receive loop error: %w", err)
				return
			}

			if deploy := msg.GetDeployTask(); deploy != nil {
				go c.handleDeployTask(deploy)
			}
		}
	}()

	// Wait for connection to end or error to occur
	select {
	case <-ctx.Done():
		return nil
	case err := <-errChan:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

// sendProgress queues task progress message to the sender channel
func (c *Client) sendProgress(taskID, state, logLine string) {
	c.activeSend <- &pb.AgentMessage{
		Payload: &pb.AgentMessage_Progress{
			Progress: &pb.TaskProgress{
				TaskId:  taskID,
				State:   state,
				LogLine: logLine,
			},
		},
	}
}

// handleDeployTask executes validation, compilation, docker deployment, and Caddy routing updates.
func (c *Client) handleDeployTask(task *pb.DeployTask) {
	log.Printf("[Agent] Starting deployment task: %s (Project: %s)", task.TaskId, task.ProjectName)

	// Step 1: Git Clone Simulation & Config Generation
	c.sendProgress(task.TaskId, "CLONING", "📥 Simulating git clone: "+task.GitUrl)
	time.Sleep(1 * time.Second)

	tempSourceDir, err := os.MkdirTemp("", "gopass_src_"+task.ProjectName+"_*")
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Failed to create temporary source directory: "+err.Error())
		return
	}
	defer os.RemoveAll(tempSourceDir)

	envJSON, _ := json.Marshal(task.Env)
	deployJSONContent := fmt.Sprintf(`{
		"project_name": "%s",
		"routing": {
			"domain": "%s",
			"container_port": %d
		},
		"env": %s
	}`, task.ProjectName, task.RoutingDomain, task.RoutingPort, string(envJSON))

	err = os.WriteFile(filepath.Join(tempSourceDir, "deploy.json"), []byte(deployJSONContent), 0644)
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Failed to generate deploy.json: "+err.Error())
		return
	}

	// Validate deploy.json
	cfg, err := types.ValidateDeployConfig([]byte(deployJSONContent))
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Validation of deploy.json failed: "+err.Error())
		return
	}
	c.sendProgress(task.TaskId, "CLONING", "✅ deploy.json validated successfully.")

	// Step 2: Build Image using Railpack/Fallback
	c.sendProgress(task.TaskId, "BUILDING", "🛠️ Compiling and building Docker image...")
	builder := NewBuilder(false) // Will auto-detect or mock
	imageTag, err := builder.BuildImage(cfg.ProjectName, tempSourceDir)
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Image compilation failed: "+err.Error())
		return
	}
	c.sendProgress(task.TaskId, "BUILDING", "✅ Docker image built: "+imageTag)

	// Step 3: Deploy Container using Docker SDK
	c.sendProgress(task.TaskId, "ROUTING", "🐳 Spawning container via Docker Manager...")
	dm := GetContainerManager()
	
	// Default owner uid set to 88888888 for testing
	hostPort, containerID, err := dm.DeployContainer(cfg.ProjectName, imageTag, cfg.Routing.ContainerPort, cfg.Env, 88888888, cfg.Routing.Domain)
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Failed to deploy Docker container: "+err.Error())
		return
	}
	c.sendProgress(task.TaskId, "ROUTING", fmt.Sprintf("✅ Container spawned (Port: %d, ID: %s)", hostPort, containerID[:12]))

	// Step 4: Refresh Caddy Route
	c.sendProgress(task.TaskId, "ROUTING", "🌐 Updating reverse proxy routing in Caddy...")
	cm := NewCaddyManager()
	if err := cm.UpdateRoute(cfg.Routing.Domain, hostPort, task.UseSsl); err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Failed to update Caddy routing: "+err.Error())
		return
	}
	c.sendProgress(task.TaskId, "ROUTING", "✅ Caddy routing configuration updated.")

	// Step 5: Smooth Transition & Clean Old Containers
	c.sendProgress(task.TaskId, "ROUTING", "🧹 Disposing of previous project containers...")
	if err := dm.CleanOldContainers(cfg.ProjectName, containerID); err != nil {
		log.Printf("[Agent] Warning: CleanOldContainers failed: %v", err)
	}

	protocolStr := "http"
	if task.UseSsl {
		protocolStr = "https"
	}
	c.sendProgress(task.TaskId, "SUCCESS", fmt.Sprintf("🎉 Deployed successfully! App live at %s://%s", protocolStr, cfg.Routing.Domain))
}

// getSystemStats retrieves host CPU, memory, and disk usage statistics
func getSystemStats() (cpuPercent, memPercent, diskPercent float64) {
	// 1. Memory Stats
	m, err := mem.VirtualMemory()
	if err == nil {
		memPercent = m.UsedPercent
	}

	// 2. CPU Stats (1-second sampling to fetch instant usage)
	cPercents, err := cpu.Percent(500*time.Millisecond, false)
	if err == nil && len(cPercents) > 0 {
		cpuPercent = cPercents[0]
	}

	// 3. Disk Stats
	d, err := disk.Usage("/")
	if err == nil {
		diskPercent = d.UsedPercent
	} else {
		// Fallback for Windows environment
		d, err = disk.Usage("C:")
		if err == nil {
			diskPercent = d.UsedPercent
		}
	}

	return
}
