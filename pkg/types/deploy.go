package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

var (
	ProjectNameRegex = regexp.MustCompile(`^[a-z0-9-]+$`)
	DomainRegex      = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)
)

// RoutingConfig holds domain and port info for routing
type RoutingConfig struct {
	Domain        string `json:"domain"`
	ContainerPort int    `json:"container_port"`
}

// DeployConfig maps the structure of deploy.json
type DeployConfig struct {
	ProjectName string            `json:"project_name"`
	Routing     RoutingConfig     `json:"routing"`
	Env         map[string]string `json:"env"`
}

// ValidateDeployConfig parses JSON bytes and runs strict validation checks
func ValidateDeployConfig(data []byte) (*DeployConfig, error) {
	var cfg DeployConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid json format: %w", err)
	}

	// 1. Validate project_name
	if cfg.ProjectName == "" {
		return nil, errors.New("project_name is required")
	}
	if len(cfg.ProjectName) > 63 {
		return nil, errors.New("project_name cannot exceed 63 characters")
	}
	if !ProjectNameRegex.MatchString(cfg.ProjectName) {
		return nil, fmt.Errorf("project_name '%s' is invalid (only lowercase letters, numbers, and hyphens allowed)", cfg.ProjectName)
	}

	// 2. Validate routing domain
	if cfg.Routing.Domain == "" {
		return nil, errors.New("routing domain is required")
	}
	if !DomainRegex.MatchString(cfg.Routing.Domain) {
		return nil, fmt.Errorf("routing domain '%s' contains invalid characters", cfg.Routing.Domain)
	}

	// 3. Validate routing port
	port := cfg.Routing.ContainerPort
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid container_port %d (must be between 1 and 65535)", port)
	}

	return &cfg, nil
}
