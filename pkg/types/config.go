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
	TLSEnabled    bool   `yaml:"tls_enabled"`
	TLSCertPath   string `yaml:"tls_cert_path"`
	TLSKeyPath    string `yaml:"tls_key_path"`
}

// AgentConfig defines settings for gopass-agent
type AgentConfig struct {
	MasterAddr         string `yaml:"master_addr"`
	NodeAlias          string `yaml:"node_alias"`
	CommunicationToken string `yaml:"communication_token"`
	TLSEnabled         bool   `yaml:"tls_enabled"`
	TLSCAPath          string `yaml:"tls_ca_path"`
	TLSSkipVerify      bool   `yaml:"tls_skip_verify"`
}

// DefaultMasterConfig returns pre-populated default values
func DefaultMasterConfig() *MasterConfig {
	return &MasterConfig{
		DbPath:        "gopass-master.db",
		GrpcAddr:      "127.0.0.1:50051",
		TelegramToken: "YOUR_TELEGRAM_BOT_TOKEN_HERE",
		TLSEnabled:    false,
		TLSCertPath:   "",
		TLSKeyPath:    "",
	}
}

// DefaultAgentConfig returns pre-populated default values
func DefaultAgentConfig() *AgentConfig {
	return &AgentConfig{
		MasterAddr:         "127.0.0.1:50051",
		NodeAlias:          "test-server-1",
		CommunicationToken: "YOUR_COMMUNICATION_TOKEN_FROM_MASTER_HERE",
		TLSEnabled:         false,
		TLSCAPath:          "",
		TLSSkipVerify:      false,
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

	// Environment variable overrides
	if envTG := os.Getenv("GOPASS_TELEGRAM_TOKEN"); envTG != "" {
		cfg.TelegramToken = envTG
	}
	if envAddr := os.Getenv("GOPASS_GRPC_ADDR"); envAddr != "" {
		cfg.GrpcAddr = envAddr
	}
	if envDb := os.Getenv("GOPASS_DB_PATH"); envDb != "" {
		cfg.DbPath = envDb
	}
	if envTLS := os.Getenv("GOPASS_TLS_ENABLED"); envTLS != "" {
		cfg.TLSEnabled = (envTLS == "true" || envTLS == "1")
	}
	if envCert := os.Getenv("GOPASS_TLS_CERT_PATH"); envCert != "" {
		cfg.TLSCertPath = envCert
	}
	if envKey := os.Getenv("GOPASS_TLS_KEY_PATH"); envKey != "" {
		cfg.TLSKeyPath = envKey
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

	// Environment variable overrides
	if envMaster := os.Getenv("GOPASS_MASTER_ADDR"); envMaster != "" {
		cfg.MasterAddr = envMaster
	}
	if envAlias := os.Getenv("GOPASS_NODE_ALIAS"); envAlias != "" {
		cfg.NodeAlias = envAlias
	}
	if envToken := os.Getenv("GOPASS_COMMUNICATION_TOKEN"); envToken != "" {
		cfg.CommunicationToken = envToken
	}
	if envTLS := os.Getenv("GOPASS_TLS_ENABLED"); envTLS != "" {
		cfg.TLSEnabled = (envTLS == "true" || envTLS == "1")
	}
	if envCA := os.Getenv("GOPASS_TLS_CA_PATH"); envCA != "" {
		cfg.TLSCAPath = envCA
	}
	if envSkip := os.Getenv("GOPASS_TLS_SKIP_VERIFY"); envSkip != "" {
		cfg.TLSSkipVerify = (envSkip == "true" || envSkip == "1")
	}

	return &cfg, nil
}
