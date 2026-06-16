package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopass/pkg/db"
	"gopass/pkg/master"
	"gopass/pkg/types"
)

func main() {
	logFile, err := os.OpenFile("gopass-master.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		mw := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(mw)
	}

	log.Println("==================================================")
	log.Println("           GOPASS MASTER 控制端服务启动")
	log.Println("==================================================")

	configPath := flag.String("config", "master.yaml", "Path to master configuration file")
	grpcAddrFlag := flag.String("grpc-addr", "", "gRPC listen address (override config)")
	tgTokenFlag := flag.String("tg-token", "", "Telegram bot token (override config)")
	tlsEnabledFlag := flag.String("tls-enabled", "", "Enable gRPC TLS (true/false) (override config)")
	tlsCertFlag := flag.String("tls-cert", "", "Path to TLS certificate file (override config)")
	tlsKeyFlag := flag.String("tls-key", "", "Path to TLS key file (override config)")
	githubTokenFlag := flag.String("github-token", "", "GitHub token for private repository releases (override config)")
	testFlag := flag.Bool("test", false, "Run health check and exit (used for OTA updates)")
	updateChatIDFlag := flag.Int64("update-success-chat-id", 0, "Chat ID to notify upon successful startup")
	flag.Parse()

	if *testFlag {
		os.Exit(0)
	}

	// 1. Load YAML configuration
	log.Printf("[Init] Loading configuration from '%s'...", *configPath)
	cfg, err := types.LoadMasterConfig(*configPath)
	if err != nil {
		log.Fatalf("[FATAL] Configuration error: %v", err)
	}

	// Override with CLI flags if provided
	if *grpcAddrFlag != "" {
		cfg.GrpcAddr = *grpcAddrFlag
	}
	if *tgTokenFlag != "" {
		cfg.TelegramToken = *tgTokenFlag
	}
	if *tlsEnabledFlag != "" {
		cfg.TLSEnabled = (*tlsEnabledFlag == "true" || *tlsEnabledFlag == "1")
	}
	if *tlsCertFlag != "" {
		cfg.TLSCertPath = *tlsCertFlag
	}
	if *tlsKeyFlag != "" {
		cfg.TLSKeyPath = *tlsKeyFlag
	}
	if *githubTokenFlag != "" {
		cfg.GithubToken = *githubTokenFlag
	}

	log.Printf("[Init] Loaded settings: DB Path='%s', gRPC Server='%s'", cfg.DbPath, cfg.GrpcAddr)

	// 2. Initialize DB Manager
	mgr, err := db.NewManager(cfg.DbPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer mgr.Close()

	// 3. Check if admin UID is configured
	var adminUID int64
	adminUID, err = mgr.GetAdminUID()
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Check if environment variable is set for non-interactive init
			envUID := os.Getenv("GOPASS_ADMIN_UID")
			if envUID != "" {
				uid, err := strconv.ParseInt(strings.TrimSpace(envUID), 10, 64)
				if err == nil && uid > 0 {
					adminUID = uid
					log.Printf("Initializing from environment variable GOPASS_ADMIN_UID: %d", adminUID)
					if err := mgr.SetAdminUID(adminUID); err != nil {
						log.Fatalf("Failed to save admin config: %v", err)
					}
					adminUser := &types.User{
						UID:      adminUID,
						Role:     "master",
						JoinedAt: time.Now(),
					}
					if err := mgr.SaveUser(adminUser); err != nil {
						log.Fatalf("Failed to register master user: %v", err)
					}
				} else {
					log.Printf("Warning: GOPASS_ADMIN_UID environment variable value '%s' is invalid, ignoring", envUID)
				}
			}

			// If environment variable wasn't set or was invalid, enable auto TG bootstrap
			if adminUID == 0 {
				log.Println("[Init] 数据库中未发现管理员 UID。系统将开启动态 Bot 首发绑定机制。")
				log.Println("[Init] 首位向 Telegram Bot 发送任意指令的用户将自动绑定为最高管理员。")
			}
		} else {
			log.Fatalf("Failed to query admin UID: %v", err)
		}
	} else {
		log.Printf("Loaded existing administrator UID: %d", adminUID)
	}

	// 4. Start gRPC Server
	server, err := master.NewServer(mgr, cfg.GrpcAddr, cfg.TLSEnabled, cfg.TLSCertPath, cfg.TLSKeyPath)
	if err != nil {
		log.Fatalf("Failed to initialize Master Server: %v", err)
	}
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start Master Server: %v", err)
	}
	defer server.Stop()

	// 5. Retrieve Communication Token for display
	commToken, err := mgr.GetCommunicationToken()
	if err == nil {
		log.Println("==================================================")
		log.Printf("  当前控制端活跃 Communication Token 为:")
		log.Printf("  >> %s <<", commToken)
		log.Println("  请在各 Agent 节点的 agent.yaml 中配置此 Token。")
		log.Println("==================================================")
	}

	// 6. Start Telegram ChatOps Bot
	useMock := cfg.TelegramToken == "" || strings.Contains(cfg.TelegramToken, "YOUR_TELEGRAM_BOT_TOKEN_HERE")
	if useMock {
		log.Println("[Init] Telegram Token is empty or default template. Starting Telegram Bot in MOCK/SIMULATION mode.")
	} else {
		log.Println("[Init] Telegram Token detected. Starting Telegram Bot in REAL mode...")
	}

	bot, err := master.NewBot(mgr, server, cfg.TelegramToken, cfg.GithubToken, useMock)
	if err != nil {
		log.Fatalf("Failed to initialize Telegram Bot: %v", err)
	}

	if *updateChatIDFlag > 0 {
		go bot.SendUpdateSuccessNotification(*updateChatIDFlag)
	}

	bot.Start()
	defer bot.Stop()

	log.Println("==================================================")
	log.Println("          Master 服务已启动并在持续运行！")
	log.Println("          退出请使用 Ctrl+C")
	log.Println("==================================================")

	// 7. Handle signal termination for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("[Shutdown] Stopping Master gRPC server & Telegram Bot...")
	log.Println("[Shutdown] Done. Goodbye!")
}
