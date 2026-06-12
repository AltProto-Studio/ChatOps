package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gopass/pkg/agent"
	"gopass/pkg/types"
)

func main() {
	log.Println("==================================================")
	log.Println("           GOPASS AGENT 被控端服务启动")
	log.Println("==================================================")

	configPath := flag.String("config", "agent.yaml", "Path to agent configuration file")
	masterFlag := flag.String("master", "", "Master address (override config)")
	tokenFlag := flag.String("token", "", "Communication token (override config)")
	aliasFlag := flag.String("alias", "", "Node alias name (override config)")
	tlsEnabledFlag := flag.String("tls-enabled", "", "Enable gRPC TLS (true/false) (override config)")
	tlsSkipVerifyFlag := flag.String("tls-skip-verify", "", "Skip gRPC TLS verification (true/false) (override config)")
	tlsCAFlag := flag.String("tls-ca", "", "Path to CA certificate file (override config)")
	flag.Parse()

	// 1. Load YAML configuration
	log.Printf("[Init] Loading configuration from '%s'...", *configPath)
	cfg, err := types.LoadAgentConfig(*configPath)
	if err != nil {
		log.Fatalf("[FATAL] Configuration error: %v", err)
	}

	// Override with CLI flags if provided
	if *masterFlag != "" {
		cfg.MasterAddr = *masterFlag
	}
	if *tokenFlag != "" {
		cfg.CommunicationToken = *tokenFlag
	}
	if *aliasFlag != "" {
		cfg.NodeAlias = *aliasFlag
	}
	if *tlsEnabledFlag != "" {
		cfg.TLSEnabled = (*tlsEnabledFlag == "true" || *tlsEnabledFlag == "1")
	}
	if *tlsSkipVerifyFlag != "" {
		cfg.TLSSkipVerify = (*tlsSkipVerifyFlag == "true" || *tlsSkipVerifyFlag == "1")
	}
	if *tlsCAFlag != "" {
		cfg.TLSCAPath = *tlsCAFlag
	}

	log.Printf("[Init] Loaded settings: Node Alias='%s', Master='%s'", cfg.NodeAlias, cfg.MasterAddr)

	// 2. Start Agent Client
	log.Printf("[Init] Starting Agent '%s' and connecting to Master...", cfg.NodeAlias)
	client := agent.NewClient(cfg.NodeAlias, cfg.CommunicationToken, cfg.MasterAddr, cfg.TLSEnabled, cfg.TLSCAPath, cfg.TLSSkipVerify)
	
	// Start client reconnection loop in a separate goroutine
	go client.Start()

	log.Println("==================================================")
	log.Println("          Agent 已成功运行！退出请使用 Ctrl+C")
	log.Println("==================================================")

	// 3. Handle signal termination for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[Shutdown] Stopping Agent client daemon...")
	client.Stop()
	log.Println("[Shutdown] Done. Goodbye!")
}
