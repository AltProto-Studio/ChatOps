package agent

import (
	"context"
	"fmt"
	"log"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// ContainerManager defines the lifecycle interface for deploying apps on Docker
type ContainerManager interface {
	DeployContainer(project string, image string, containerPort int, env map[string]string, ownerUID int64, domain string) (int, string, error)
	CleanOldContainers(project string, keepContainerID string) error
}

// GetContainerManager automatically connects to the Docker API. If Docker is unavailable,
// it falls back to the in-memory Mock Container Manager.
func GetContainerManager() ContainerManager {
	mgr, err := NewRealDockerManager()
	if err != nil {
		log.Printf("[Docker] Connection failed: %v. Falling back to Mock Docker Engine.", err)
		return NewMockDockerManager()
	}
	log.Println("[Docker] Successfully connected to host Docker Daemon.")
	return mgr
}

// --- Real Docker Implementation ---

type RealDockerManager struct {
	cli *client.Client
}

func NewRealDockerManager() (*RealDockerManager, error) {
	var host string
	if runtime.GOOS == "windows" {
		host = "npipe:////./pipe/docker_engine"
	} else {
		host = "unix:///var/run/docker.sock"
	}

	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker ping failed: %w", err)
	}

	return &RealDockerManager{cli: cli}, nil
}

func (m *RealDockerManager) DeployContainer(project string, image string, containerPort int, env map[string]string, ownerUID int64, domain string) (int, string, error) {
	ctx := context.Background()

	// Convert env map to slice
	var envList []string
	for k, v := range env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	portStr := strconv.Itoa(containerPort)
	natPort, err := nat.NewPort("tcp", portStr)
	if err != nil {
		return 0, "", err
	}

	containerConfig := &container.Config{
		Image: image,
		Env:   envList,
		Labels: map[string]string{
			"gopass.managed":      "true",
			"gopass.project_name": project,
			"gopass.owner_uid":    strconv.FormatInt(ownerUID, 10),
			"gopass.domain":       domain,
		},
		ExposedPorts: nat.PortSet{
			natPort: struct{}{},
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			natPort: []nat.PortBinding{
				{
					HostIP:   "0.0.0.0",
					HostPort: "", // Let Docker choose an empty port
				},
			},
		},
	}

	containerName := fmt.Sprintf("gopass-%s-%d", project, time.Now().Unix())

	resp, err := m.cli.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		return 0, "", fmt.Errorf("failed to create container: %w", err)
	}

	err = m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{})
	if err != nil {
		return 0, "", fmt.Errorf("failed to start container: %w", err)
	}

	// Retrieve allocated host port
	inspect, err := m.cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		return 0, "", fmt.Errorf("failed to inspect container: %w", err)
	}

	bindings, ok := inspect.NetworkSettings.Ports[natPort]
	if !ok || len(bindings) == 0 {
		return 0, "", fmt.Errorf("no port bindings found for container")
	}

	hostPort, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil {
		return 0, "", fmt.Errorf("failed to parse host port: %w", err)
	}

	// Wait for container socket to listen (mock health check)
	m.waitForPort("127.0.0.1", hostPort)

	return hostPort, resp.ID, nil
}

func (m *RealDockerManager) CleanOldContainers(project string, keepContainerID string) error {
	ctx := context.Background()

	args := filters.NewArgs()
	args.Add("label", fmt.Sprintf("gopass.project_name=%s", project))
	args.Add("label", "gopass.managed=true")

	containers, err := m.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return err
	}

	for _, c := range containers {
		if c.ID == keepContainerID {
			continue
		}

		log.Printf("[Docker] Stopping and removing old container: %s (ID: %s)", c.Names[0], c.ID)

		// Stop container
		timeout := 10 // seconds
		stopOpts := container.StopOptions{
			Timeout: &timeout,
		}
		if err := m.cli.ContainerStop(ctx, c.ID, stopOpts); err != nil {
			log.Printf("[Docker] Warning: failed to stop container %s: %v", c.ID, err)
		}

		// Remove container
		removeOpts := container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		}
		if err := m.cli.ContainerRemove(ctx, c.ID, removeOpts); err != nil {
			log.Printf("[Docker] Warning: failed to remove container %s: %v", c.ID, err)
		}
	}

	return nil
}

func (m *RealDockerManager) waitForPort(ip string, port int) {
	addr := fmt.Sprintf("%s:%d", ip, port)
	for i := 0; i < 10; i++ {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// --- Mock Docker Implementation ---

type MockContainer struct {
	ID        string
	Project   string
	Image     string
	HostPort  int
	OwnerUID  int64
	Domain    string
	Connected bool
}

type MockDockerManager struct {
	mu         sync.Mutex
	containers map[string]*MockContainer
	portIndex  int
}

func NewMockDockerManager() *MockDockerManager {
	return &MockDockerManager{
		containers: make(map[string]*MockContainer),
		portIndex:  32768,
	}
}

func (m *MockDockerManager) DeployContainer(project string, image string, containerPort int, env map[string]string, ownerUID int64, domain string) (int, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	hostPort := m.portIndex
	m.portIndex++

	containerID := fmt.Sprintf("mock-container-%s-%d", project, time.Now().UnixNano())

	log.Printf("[MockDocker] Creating container '%s' mapping port %d -> %d...", containerID, hostPort, containerPort)
	log.Printf("[MockDocker] Labels: gopass.managed=true, gopass.project_name=%s, gopass.owner_uid=%d, gopass.domain=%s", project, ownerUID, domain)

	m.containers[containerID] = &MockContainer{
		ID:        containerID,
		Project:   project,
		Image:     image,
		HostPort:  hostPort,
		OwnerUID:  ownerUID,
		Domain:    domain,
		Connected: true,
	}

	return hostPort, containerID, nil
}

func (m *MockDockerManager) CleanOldContainers(project string, keepContainerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, c := range m.containers {
		if c.Project == project && id != keepContainerID {
			log.Printf("[MockDocker] Stopping and removing old container: %s", id)
			c.Connected = false
			delete(m.containers, id)
		}
	}
	return nil
}
