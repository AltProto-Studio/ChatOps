package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	pb "gopass/pkg/proto"
	"gopass/pkg/types"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Client handles gRPC communication from Agent to Master
type Client struct {
	alias         string
	token         string
	masterAddr    string
	tlsEnabled    bool
	tlsCAPath     string
	tlsSkipVerify bool
	stopChan      chan struct{}
	stopOnce      sync.Once
	activeSend    chan *pb.AgentMessage
}

// NewClient creates a new Agent Client instance
func NewClient(alias, token, masterAddr string, tlsEnabled bool, tlsCAPath string, tlsSkipVerify bool) *Client {
	return &Client{
		alias:         alias,
		token:         token,
		masterAddr:    masterAddr,
		tlsEnabled:    tlsEnabled,
		tlsCAPath:     tlsCAPath,
		tlsSkipVerify: tlsSkipVerify,
		stopChan:      make(chan struct{}),
		activeSend:    make(chan *pb.AgentMessage, 100),
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
	if !c.tlsEnabled {
		log.Printf("[SECURITY WARNING] TLS is disabled. Agent gRPC connection to Master is unencrypted and susceptible to MITM.")
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		if c.tlsSkipVerify {
			log.Printf("[SECURITY WARNING] TLS InsecureSkipVerify is TRUE. Agent gRPC connection is susceptible to MITM.")
		}
		tlsConfig := &tls.Config{
			InsecureSkipVerify: c.tlsSkipVerify,
		}
		if c.tlsCAPath != "" {
			caCert, err := os.ReadFile(c.tlsCAPath)
			if err != nil {
				return fmt.Errorf("failed to read CA certificate file: %w", err)
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return fmt.Errorf("failed to parse CA certificate from %s", c.tlsCAPath)
			}
			tlsConfig.RootCAs = caCertPool
		}
		creds := credentials.NewTLS(tlsConfig)
		opts = append(opts, grpc.WithTransportCredentials(creds))
		log.Printf("[SECURITY] gRPC TLS client enabled (SkipVerify: %v, CAPath: %s)", c.tlsSkipVerify, c.tlsCAPath)
	}

	conn, err := grpc.NewClient(c.masterAddr, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to master: %w", err)
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
			} else if update := msg.GetUpdateAgentTask(); update != nil {
				go c.handleUpdateTask(update)
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

	// Step 1: Real Git Clone
	c.sendProgress(task.TaskId, "CLONING", "📥 Cloning git repository: "+task.GitUrl)

	tempSourceDir, err := os.MkdirTemp("", "gopass_src_"+task.ProjectName+"_*")
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Failed to create temporary source directory: "+err.Error())
		return
	}
	defer os.RemoveAll(tempSourceDir)

	cmd := exec.Command("git", "clone", "--depth=1", task.GitUrl, tempSourceDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Git clone failed: "+string(out))
		return
	}

	commitCmd := exec.Command("git", "rev-parse", "HEAD")
	commitCmd.Dir = tempSourceDir
	commitHashBytes, err := commitCmd.Output()
	var commitHash string
	if err == nil {
		commitHash = strings.TrimSpace(string(commitHashBytes))
		c.sendProgress(task.TaskId, "CLONING", "✅ Git clone successful. Commit: "+commitHash)
	} else {
		commitHash = fmt.Sprintf("%d", time.Now().Unix())
	}

	// We pass the commitHash via environment so the builder can use it as a tag
	if task.Env == nil {
		task.Env = make(map[string]string)
	}
	task.Env["GOPASS_COMMIT_HASH"] = commitHash

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
	builder := NewBuilder(false) // Will auto-detect or mock
	
	// Estimate build time and notify master of restricted mode
	estimatedTime, reason := builder.EstimateBuildTime()
	c.sendProgress(task.TaskId, "BUILDING", fmt.Sprintf("🛠️ 准备编译... (预估用时: %s)\n%s", estimatedTime, reason))
	
	imageTag, existed, err := builder.BuildImage(cfg.ProjectName, tempSourceDir, commitHash)
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Image compilation failed: "+err.Error())
		return
	}
	if existed {
		c.sendProgress(task.TaskId, "BUILDING", "✅ Docker image already exists (cached): "+imageTag)
	} else {
		c.sendProgress(task.TaskId, "BUILDING", "✅ Docker image built: "+imageTag)
	}

	// Step 3: Deploy Container using Docker SDK
	c.sendProgress(task.TaskId, "ROUTING", "🐳 Spawning container via Docker Manager...")
	dm := GetContainerManager()
	
	hostPort, containerID, err := dm.DeployContainer(cfg.ProjectName, imageTag, cfg.Routing.ContainerPort, cfg.Env, 88888888, cfg.Routing.Domain)
	if err != nil {
		c.sendProgress(task.TaskId, "FAILED", "❌ Container deployment failed: "+err.Error())
		return
	}

	c.sendProgress(task.TaskId, "ROUTING", fmt.Sprintf("✅ Container deployed successfully. Bound to host port %d", hostPort))

	caddy := NewCaddyManager()
	if cfg.Routing.Domain != "" {
		c.sendProgress(task.TaskId, "ROUTING", "🌐 Configuring Caddy reverse proxy for domain: "+cfg.Routing.Domain)
		if err := caddy.UpdateRoute(cfg.Routing.Domain, hostPort, task.UseSsl); err != nil {
			c.sendProgress(task.TaskId, "WARNING", "⚠️ Caddy routing configuration failed: "+err.Error())
		} else {
			c.sendProgress(task.TaskId, "ROUTING", "✅ Caddy routing configured successfully.")
		}
	} else {
		c.sendProgress(task.TaskId, "ROUTING", "🌐 No custom domain specified. Direct IP access only.")
	}

	// Send final JSON state back for DB tracking
	finalState := struct {
		Domain string `json:"domain"`
		Port   int    `json:"port"`
	}{
		Domain: cfg.Routing.Domain,
		Port:   hostPort,
	}
	finalData, _ := json.Marshal(finalState)
	c.sendProgress(task.TaskId, "DEPLOY_SUCCESS", string(finalData))

	c.sendProgress(task.TaskId, "COMPLETED", "🎉 Application deployed and is now LIVE!")

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

// handleUpdateTask updates the Agent binary and restarts
func (c *Client) handleUpdateTask(task *pb.UpdateAgentTask) {
	log.Printf("[Agent] Received update task from Master. Download URL template: %s", task.DownloadUrl)

	resolvedURL := task.DownloadUrl
	resolvedURL = strings.ReplaceAll(resolvedURL, "{{OS}}", runtime.GOOS)
	resolvedURL = strings.ReplaceAll(resolvedURL, "{{ARCH}}", runtime.GOARCH)
	if runtime.GOOS == "windows" {
		resolvedURL = strings.ReplaceAll(resolvedURL, "{{EXT}}", ".exe")
	} else {
		resolvedURL = strings.ReplaceAll(resolvedURL, "{{EXT}}", "")
	}

	log.Printf("[Agent] Resolved download URL: %s", resolvedURL)

	err := DownloadAndReplaceAgentBinary(resolvedURL, task.GithubToken, task.TagName)
	if err != nil {
		log.Printf("[Agent] Update failed: %v", err)
		return
	}

	log.Println("[Agent] Update completed successfully. Re-spawning new agent and exiting...")
	c.Stop()

	err = RestartAgent()
	if err != nil {
		log.Printf("[Agent] Failed to restart: %v", err)
	}
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
