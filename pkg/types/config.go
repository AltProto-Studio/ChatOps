package types

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// MasterConfig defines settings for gopass-master
type MasterConfig struct {
	DbPath        string `yaml:"db_path"`
	GrpcAddr      string `yaml:"grpc_addr"`
	TelegramToken string `yaml:"telegram_token"`
}

// AgentConfig defines settings for gopass-agent
type AgentConfig struct {
	MasterAddr         string `yaml:"master_addr"`
	NodeAlias          string `yaml:"node_alias"`
	CommunicationToken string `yaml:"communication_token"`
}

// DefaultMasterConfig returns pre-populated default values
func DefaultMasterConfig() *MasterConfig {
	return &MasterConfig{
		DbPath:        "gopass-master.db",
		GrpcAddr:      "127.0.0.1:50051",
		TelegramToken: "YOUR_TELEGRAM_BOT_TOKEN_HERE",
	}
}

// DefaultAgentConfig returns pre-populated default values
func DefaultAgentConfig() *AgentConfig {
	return &AgentConfig{
		MasterAddr:         "127.0.0.1:50051",
		NodeAlias:          "test-server-1",
		CommunicationToken: "YOUR_COMMUNICATION_TOKEN_FROM_MASTER_HERE",
	}
}

// LoadMasterConfig reads Master YAML config from file, or auto-generates one if missing
func LoadMasterConfig(path string) (*MasterConfig, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// File does not exist, auto-generate defaults
		cfg := DefaultMasterConfig()
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal default master config: %w", err)
		}
		
		commentedHeader := []byte("# GOPASS Master Configuration File\n# Please fill in your Telegram Token to launch the bot.\n\n")
		fullData := append(commentedHeader, data...)
		
		if err := os.WriteFile(path, fullData, 0644); err != nil {
			return nil, fmt.Errorf("failed to create default master config: %w", err)
		}
		return nil, fmt.Errorf("config file '%s' was missing. A default template has been generated. Please edit it and restart", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg MasterConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse yaml config: %w", err)
	}

	return &cfg, nil
}

// LoadAgentConfig reads Agent YAML config from file, or auto-generates one if missing
func LoadAgentConfig(path string) (*AgentConfig, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// File does not exist, auto-generate defaults
		cfg := DefaultAgentConfig()
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal default agent config: %w", err)
		}
		
		commentedHeader := []byte("# GOPASS Agent Configuration File\n# Please fill in the Communication Token assigned by Master.\n\n")
		fullData := append(commentedHeader, data...)
		
		if err := os.WriteFile(path, fullData, 0644); err != nil {
			return nil, fmt.Errorf("failed to create default agent config: %w", err)
		}
		return nil, fmt.Errorf("config file '%s' was missing. A default template has been generated. Please edit it and restart", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse yaml config: %w", err)
	}

	return &cfg, nil
}
