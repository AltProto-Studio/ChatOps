package master

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopass/pkg/cloudflare"
	"gopass/pkg/db"
	pb "gopass/pkg/proto"
	"gopass/pkg/types"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// UserConversationState stores current interactive wizard state for a user
type UserConversationState struct {
	Step        string    // "IDLE", "WAITING_FOR_GIT_URL", etc.
	NodeAlias   string    // Target node alias chosen for deploy
	UpdatedAt   time.Time
	PromptMsgID int       // Bot's interactive prompt/error panel message ID
	UserMsgIDs  []int     // List of user-sent inputs (errors, values) to be swept clean

	// Temporary deploy wizard configuration properties
	DeployGitURL    string
	DeployDomain    string
	BaseDomain      string
	DeployCFDNSType string // "proxy", "dns", "none"
	DeployUseSSL    bool

	// Temporary SSH node addition wizard properties
	SSHHost      string
	SSHPort      int
	SSHUser      string
	SSHAuth      string // Password or private key
	SSHNodeAlias string

	// Temporary CF Config properties
	CFToken string
	CFZone  string
	CFName  string

	CFR2AccessKeyID     string
	CFR2SecretAccessKey string
	CFR2Endpoint        string

	TargetUID int64 // Used for CF Allocation and Admin setup
}

// DeploymentSession stores details of an ongoing deploy task for Telegram updates
type DeploymentSession struct {
	ChatID      int64
	MessageID   int
	NodeAlias   string
	ProjectName string
	Domain      string
	LogLines    []string
	LastUpdated time.Time
	UserMsgID   int       // User's git url input message ID to sweep clean
	UseSSL      bool
	CFDNSType   string    // "proxy", "dns", "none"
	OwnerUID    int64
}

// Bot handles Telegram ChatOps commands and updates
type Bot struct {
	dbManager    *db.Manager
	gRPCServer   *Server
	token        string
	githubToken  string
	api          *tgbotapi.BotAPI
	useMock      bool
	mu           sync.Mutex
	lastEdit     map[int]time.Time // MessageID -> Last edit timestamp
	sessionsMu   sync.Mutex
	sessions     map[string]*DeploymentSession // taskID -> Session
	qc           *QueueCoordinator
	stopChan     chan struct{}
	userStatesMu sync.Mutex
	userStates   map[int64]*UserConversationState // fromUID -> State
}

// NewBot initializes a new Bot instance
func NewBot(dbManager *db.Manager, gRPCServer *Server, token string, githubToken string, useMock bool) (*Bot, error) {
	var api *tgbotapi.BotAPI
	var err error

	if !useMock && token != "" {
		api, err = tgbotapi.NewBotAPI(token)
		if err != nil {
			return nil, fmt.Errorf("failed to create telegram bot: %w", err)
		}
		log.Printf("[Bot] Authorized on Telegram account: %s", api.Self.UserName)
	}

	bot := &Bot{
		dbManager:    dbManager,
		gRPCServer:   gRPCServer,
		token:        token,
		githubToken:  githubToken,
		api:          api,
		useMock:      useMock || (api == nil),
		lastEdit:     make(map[int]time.Time),
		sessions:     make(map[string]*DeploymentSession),
		qc:           NewQueueCoordinator(),
		stopChan:     make(chan struct{}),
		userStates:   make(map[int64]*UserConversationState),
	}

	// Connect gRPC callbacks to Bot
	gRPCServer.SetProgressCallback(bot.onTaskProgress)
	gRPCServer.SetHeartbeatCallback(bot.onNodeHeartbeat)

	return bot, nil
}

// Start launches message polling loop (if not in mock mode)
func (b *Bot) Start() {
	if b.useMock {
		log.Println("[Bot] Running in MOCK/SIMULATION mode. Polling loop skipped.")
		return
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	go func() {
		for {
			select {
			case <-b.stopChan:
				return
			case update := <-updates:
				if update.CallbackQuery != nil {
					b.HandleCallbackQuery(update.CallbackQuery)
					continue
				}
				if update.Message == nil {
					continue
				}
				b.HandleMessage(update.Message)
			}
		}
	}()
}

// Stop terminates the bot loop
func (b *Bot) Stop() {
	close(b.stopChan)
}

// isValidGitURL checks if the string matches basic git clone repository patterns
func isValidGitURL(url string) bool {
	url = strings.TrimSpace(url)
	if url == "" {
		return false
	}
	var gitPattern = regexp.MustCompile(`^(https?://|git@|ssh://)([\w.-]+)(:|\/)[\w.-]+/[\w.-]+(\.git)?$`)
	return gitPattern.MatchString(url)
}

// clearConversationHistory physically deletes all intermediate dialog traces
func (b *Bot) clearConversationHistory(chatID int64, state *UserConversationState) {
	if state == nil {
		return
	}
	if state.PromptMsgID > 0 {
		b.deleteMessage(chatID, state.PromptMsgID)
	}
	for _, id := range state.UserMsgIDs {
		b.deleteMessage(chatID, id)
	}
}

// sendErrorReplyWithCancel sends error message with a cancel path, replacing any existing warning panel
func (b *Bot) sendErrorReplyWithCancel(chatID int64, fromUID int64, text string) {
	b.userStatesMu.Lock()
	state, hasState := b.userStates[fromUID]
	if hasState && state.PromptMsgID > 0 {
		b.deleteMessage(chatID, state.PromptMsgID)
		state.PromptMsgID = 0
	}
	b.userStatesMu.Unlock()

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		[]tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("❌ 取消操作并返回主菜单", "menu_cancel_and_back"),
		},
	)
	if b.useMock {
		log.Printf("[Bot Mock] REPLY ERROR to Chat %d: %s", chatID, text)
		return
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	sent, err := b.api.Send(msg)
	if err == nil {
		b.userStatesMu.Lock()
		if hasState {
			state.PromptMsgID = sent.MessageID
		}
		b.userStatesMu.Unlock()
	}
}

// hasPermission checks if a user is a master or an admin with the specific permission
func (b *Bot) hasPermission(user *types.User, perm string) bool {
	if user == nil {
		return false
	}
	if user.Role == "master" {
		return true // Master has all permissions
	}
	if user.Role == "admin" {
		if user.Permissions == nil {
			return false
		}
		return user.Permissions[perm]
	}
	return false
}

// HandleMessage routes and runs authorized commands and custom keyboard actions
func (b *Bot) HandleMessage(msg *tgbotapi.Message) {
	if msg == nil {
		return
	}

	chatID := msg.Chat.ID
	fromUID := msg.From.ID
	text := strings.TrimSpace(msg.Text)
	msgID := msg.MessageID

	// Extract Telegram display name
	displayName := strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)

	// 1. Authorization Middleware Check & Bootstrap first user
	user, err := b.dbManager.GetUser(fromUID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		b.reply(chatID, "❌ 数据库查询错误。")
		return
	}

	if user == nil {
		// Auto-bootstrap: if database has no admin_uid, register the very first user as master admin
		_, dbErr := b.dbManager.GetAdminUID()
		if dbErr != nil && errors.Is(dbErr, db.ErrNotFound) {
			log.Printf("[BOOTSTRAP] Database has no administrator. Registering UID %d as Master admin.", fromUID)
			_ = b.dbManager.SetAdminUID(fromUID)
			user = &types.User{
				UID:      fromUID,
				Role:     "master",
				Name:     displayName,
				JoinedAt: time.Now(),
			}
			_ = b.dbManager.SaveUser(user)
			b.reply(chatID, fmt.Sprintf("👑 **初始化成功！**\n系统检测到您是首位使用者，已自动将您的 Telegram UID (`%d`) 绑定为最高管理员主账户。", fromUID))

			// Immediately show GUI main control panel
			b.showMainMenu(chatID, user)
			return
		}
	}

	if user == nil {
		// Unregistered user authorization flow: only allow /join
		if strings.HasPrefix(text, "/join") {
			parts := strings.Fields(text)
			if len(parts) < 2 {
				b.reply(chatID, "❌ 请提供激活码。用法: `/join <激活码>`")
				return
			}
			b.handleJoin(chatID, fromUID, parts[1], msgID, displayName)
			return
		}
		// Silent drop for other messages to keep system clean
		log.Printf("[SECURITY] Unauthorized message from user UID %d ignored: %s", fromUID, text)
		return
	}

	// Auto-sync Telegram display name for registered users
	if user != nil && displayName != "" && user.Name != displayName {
		user.Name = displayName
		_ = b.dbManager.SaveUser(user)
	}

	// 全局一键取消/返回主菜单拦截
	if text == "❌ 取消操作" || text == "⬅️ 返回主菜单" || text == "/cancel" {
		if text == "❌ 取消操作" || text == "⬅️ 返回主菜单" {
			b.deleteMessage(chatID, msgID)
		}
		b.userStatesMu.Lock()
		state, hasState := b.userStates[fromUID]
		if hasState {
			delete(b.userStates, fromUID)
		}
		b.userStatesMu.Unlock()

		if hasState && state != nil {
			b.clearConversationHistory(chatID, state)
		}

		if b.useMock {
			log.Printf("[Bot Mock] REPLY: 会话操作已取消。")
		} else {
			sentMsg, err := b.api.Send(tgbotapi.NewMessage(chatID, "❌ 会话操作已取消。"))
			if err == nil {
				go func(sentID int) {
					time.Sleep(3 * time.Second)
					b.deleteMessage(chatID, sentID)
				}(sentMsg.MessageID)
			}
		}

		b.showMainMenu(chatID, user)
		return
	}

	// 2. Wizard mode interactive state interceptor
	b.userStatesMu.Lock()
	state, hasState := b.userStates[fromUID]
	b.userStatesMu.Unlock()

	if hasState && !strings.HasPrefix(text, "/") {
		if state.Step == "WAITING_FOR_NODE_SELECTION" {
			b.deleteMessage(chatID, msgID)

			if !strings.HasPrefix(text, "🖥️ ") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ **输入无效。**\n\n请在下方的键盘上点击选择一个在线节点别名:")
				return
			}
			nodeAlias := strings.TrimPrefix(text, "🖥️ ")
			nodeAlias = strings.TrimSpace(nodeAlias)

			node, err := b.dbManager.GetNode(nodeAlias)
			if err != nil || !node.Connected {
				b.sendErrorReplyWithCancel(chatID, fromUID, fmt.Sprintf("❌ 节点 `%s` 离线或不存在，请重新在下方选择:", nodeAlias))
				return
			}

			if state.PromptMsgID > 0 {
				b.deleteMessage(chatID, state.PromptMsgID)
			}

			b.userStatesMu.Lock()
			state.NodeAlias = nodeAlias
			state.UpdatedAt = time.Now()
			state.UserMsgIDs = nil
			b.userStatesMu.Unlock()

			// 下一步：选择域名类型
			b.showDomainTypeMenu(chatID, fromUID)
			return
		}

		if state.Step == "WAITING_FOR_DOMAIN_TYPE" {
			b.deleteMessage(chatID, msgID)

			if text == "🌐 直接使用节点公网 IP" {
				b.userStatesMu.Lock()
				state.DeployDomain = "" // 使用默认IP
				state.DeployCFDNSType = "none"
				state.DeployUseSSL = false
				state.Step = "WAITING_FOR_GIT_URL"
				state.UpdatedAt = time.Now()
				b.userStatesMu.Unlock()

				b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_GIT_URL", "🚀 请发送 Git 仓库地址:", b.getCancelKeyboard())
			} else if text == "☁️ 绑定 Cloudflare 域名 (推荐)" || text == "✏️ 自定义独立域名" {
				go b.handleFetchCFZones(chatID, fromUID, user)
			} else {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 输入无效，请重新选择:")
			}
			return
		}

		if state.Step == "WAITING_FOR_CF_ZONE_SELECTION" {
			b.deleteMessage(chatID, msgID)
			
			baseDomain := strings.TrimSpace(strings.TrimPrefix(text, "🌐 "))
			b.userStatesMu.Lock()
			state.Step = "WAITING_FOR_SUBDOMAIN"
			state.BaseDomain = baseDomain
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SUBDOMAIN", fmt.Sprintf("✏️ 请输入三级域名前缀 (例如输入 `api` 将绑定至 `api.%s`):", baseDomain), b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_SUBDOMAIN" {
			b.deleteMessage(chatID, msgID)

			subPrefix := strings.TrimSpace(text)
			if subPrefix == "" || strings.Contains(subPrefix, " ") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 前缀格式错误，请重新输入:")
				return
			}

			b.userStatesMu.Lock()
			state.DeployDomain = fmt.Sprintf("%s.%s", subPrefix, state.BaseDomain)
			state.Step = "WAITING_FOR_CF_DNS_TYPE"
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			b.showCFDNSTypeMenu(chatID, fromUID)
			return
		}

		if state.Step == "WAITING_FOR_CUSTOM_DOMAIN" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			cleanDomain := strings.TrimSpace(text)
			if cleanDomain == "" || strings.Contains(cleanDomain, " ") || strings.HasPrefix(cleanDomain, "/") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 域名格式错误，请重新输入:")
				return
			}

			b.userStatesMu.Lock()
			state.DeployDomain = cleanDomain
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			for _, id := range state.UserMsgIDs {
				b.deleteMessage(chatID, id)
			}
			b.userStatesMu.Lock()
			state.UserMsgIDs = nil
			b.userStatesMu.Unlock()

			// 下一步：Cloudflare 解析配置
			b.showCFDNSTypeMenu(chatID, fromUID)
			return
		}

		if state.Step == "WAITING_FOR_CF_DNS_TYPE" {
			b.deleteMessage(chatID, msgID)

			cfType := "none"
			switch text {
			case "☁️ 开启 CF 解析 + CDN Proxy":
				cfType = "proxy"
			case "☁️ 仅解析 DNS":
				cfType = "dns"
			case "☁️ 不使用 CF 解析":
				cfType = "none"
			default:
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 选择无效，请重新选择:")
				return
			}

			b.userStatesMu.Lock()
			state.DeployCFDNSType = cfType
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			// 下一步：SSL 证书选择
			b.showSSLTypeMenu(chatID, fromUID)
			return
		}

		if state.Step == "WAITING_FOR_SSL_TYPE" {
			b.deleteMessage(chatID, msgID)

			var useSSL bool
			if text == "🔒 自动 SSL (Caddy 自动申请)" {
				useSSL = true
			} else if text == "🔓 仅 HTTP 部署" {
				useSSL = false
			} else {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 选择无效，请重新选择:")
				return
			}

			b.userStatesMu.Lock()
			state.DeployUseSSL = useSSL
			b.userStatesMu.Unlock()

			// 下一步：输入 Git 地址
			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_GIT_URL", "🚀 请发送 Git 仓库地址:", b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_MASTER_UPDATE_CONFIRM" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			choice := strings.ToLower(strings.TrimSpace(text))
			if choice == "yes" || choice == "y" {
				buf := make([]byte, 2)
				_, _ = rand.Read(buf)
				captcha := fmt.Sprintf("%X", buf)

				b.userStatesMu.Lock()
				state.Step = "WAITING_FOR_MASTER_UPDATE_CAPTCHA"
				state.DeployGitURL = captcha // Reuse DeployGitURL to store captcha
				b.userStatesMu.Unlock()

				msgText := fmt.Sprintf("⚠️ **高危操作验证**\n为了防止误操作和越权执行，请回复以下信息以确认身份和意图：\n\n您的 UID: `%d`\n本次动态验证码: `%s`\n\n请严格按此格式回复 (中间用空格分隔): `UID 验证码`\n示例: `%d %s`", fromUID, captcha, fromUID, captcha)
				b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_MASTER_UPDATE_CAPTCHA", msgText, b.getCancelKeyboard())
				return

			} else if choice == "no" || choice == "n" {
				b.userStatesMu.Lock()
				delete(b.userStates, fromUID)
				b.userStatesMu.Unlock()

				if state.PromptMsgID > 0 {
					b.deleteMessage(chatID, state.PromptMsgID)
				}
				for _, id := range state.UserMsgIDs {
					b.deleteMessage(chatID, id)
				}

				msgCfg := tgbotapi.NewMessage(chatID, "已取消更新并返回主菜单。")
				msgCfg.ReplyMarkup = b.getMainMenuMarkup(user)
				_, _ = b.api.Send(msgCfg)
				return
			} else {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 输入无效。请手动输入 **yes** 或 **no** 确认是否升级 Master：")
				return
			}
		}

		if state.Step == "WAITING_FOR_MASTER_UPDATE_CAPTCHA" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			expectedCaptcha := state.DeployGitURL
			expectedText := fmt.Sprintf("%d %s", fromUID, expectedCaptcha)

			if strings.TrimSpace(text) != expectedText {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 身份验证或验证码输入错误，请重新输入:")
				return
			}

			// Log the action securely
			logMsg := fmt.Sprintf("[%s] [SECURITY] Master upgrade initiated by user %d (%s)\n", time.Now().Format(time.RFC3339), fromUID, user.Name)
			f, errLog := os.OpenFile("gopass-master.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if errLog == nil {
				f.WriteString(logMsg)
				f.Close()
			} else {
				log.Printf("Failed to write to master log: %v", errLog)
			}

			b.updateWizardPrompt(chatID, fromUID, "UPGRADING_MASTER", "🔄 正在从 GitHub 下载最新 Master 更新包，请稍候...", nil)

			release, err := FetchLatestRelease(b.githubToken)
			if err != nil {
				b.updateWizardPrompt(chatID, fromUID, "IDLE", "❌ 无法获取最新 Release: "+err.Error(), b.getMainMenuMarkup(user))
				return
			}

			asset := release.GetMatchingAsset("master")
			if asset == nil {
				b.updateWizardPrompt(chatID, fromUID, "IDLE", "❌ 未在 Release 资产中找到适配当前平台/架构的 Master 二进制文件！", b.getMainMenuMarkup(user))
				return
			}

			b.updateWizardPrompt(chatID, fromUID, "UPGRADING_MASTER", "📥 正在下载新版本 Master 二进制...", nil)
			err = DownloadAndReplaceBinary(asset, b.githubToken)
			if err != nil {
				b.updateWizardPrompt(chatID, fromUID, "IDLE", "❌ 下载并替换二进制失败: "+err.Error(), b.getMainMenuMarkup(user))
				return
			}

			b.updateWizardPrompt(chatID, fromUID, "UPGRADING_MASTER", "🎉 Master 二进制更新成功！正在重新启动服务，本聊天会话将短暂断开。请在 3 秒后发送任意消息唤醒...", nil)
			time.Sleep(1 * time.Second)

			// Cleanup state
			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			if state.PromptMsgID > 0 {
				b.deleteMessage(chatID, state.PromptMsgID)
			}
			for _, id := range state.UserMsgIDs {
				b.deleteMessage(chatID, id)
			}

			err = RestartProcess(b.dbManager, b.gRPCServer)
			if err != nil {
				b.reply(chatID, "❌ 重新启动服务失败: "+err.Error())
			}
			return
		}

		if state.Step == "WAITING_FOR_GIT_URL" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			if !isValidGitURL(text) {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ Git 仓库地址格式错误，请重新输入:")
				return
			}

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			// 物理删除所有临时向导输入气泡，确保不堆积
			if state.PromptMsgID > 0 {
				b.deleteMessage(chatID, state.PromptMsgID)
			}
			for _, id := range state.UserMsgIDs {
				b.deleteMessage(chatID, id)
			}

			b.handleDeploy(chatID, fromUID, text, state.NodeAlias, state.DeployDomain, state.DeployCFDNSType, state.DeployUseSSL, msgID)
			return
		}

		if state.Step == "WAITING_FOR_SSH_HOST" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			host := strings.TrimSpace(text)
			if host == "" || strings.Contains(host, " ") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ IP/主机名格式错误，请重新输入:")
				return
			}

			b.userStatesMu.Lock()
			state.SSHHost = host
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			portsKeyboard := tgbotapi.NewReplyKeyboard(
				tgbotapi.NewKeyboardButtonRow(
					tgbotapi.NewKeyboardButton("22"),
					tgbotapi.NewKeyboardButton("❌ 取消操作"),
				),
			)
			portsKeyboard.ResizeKeyboard = true
			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SSH_PORT", "🔌 请选择或输入 SSH 端口号 (默认 22):", portsKeyboard)
			return
		}

		if state.Step == "WAITING_FOR_SSH_PORT" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			portVal := strings.TrimSpace(text)
			port := 22
			if portVal != "" {
				var p int
				_, err := fmt.Sscanf(portVal, "%d", &p)
				if err != nil || p <= 0 || p > 65535 {
					b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 端口号无效，请输入 1 到 65535 之间的数字:")
					return
				}
				port = p
			}

			b.userStatesMu.Lock()
			state.SSHPort = port
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			usersKeyboard := tgbotapi.NewReplyKeyboard(
				tgbotapi.NewKeyboardButtonRow(
					tgbotapi.NewKeyboardButton("root"),
					tgbotapi.NewKeyboardButton("❌ 取消操作"),
				),
			)
			usersKeyboard.ResizeKeyboard = true
			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SSH_USER", "👤 请选择或输入 SSH 用户名 (默认 root):", usersKeyboard)
			return
		}

		if state.Step == "WAITING_FOR_SSH_USER" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			userVal := strings.TrimSpace(text)
			if userVal == "" {
				userVal = "root"
			}

			b.userStatesMu.Lock()
			state.SSHUser = userVal
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SSH_AUTH", "🔑 请发送 SSH 登录密码 或 私钥 PEM 证书内容:\n\n*(提示：系统将在收到后立即物理删除该条消息，绝对安全)*", b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_SSH_AUTH" {
			b.deleteMessage(chatID, msgID)

			authVal := text
			if authVal == "" {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 密码或私钥不能为空，请重新发送:")
				return
			}

			b.userStatesMu.Lock()
			state.SSHAuth = authVal
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SSH_ALIAS", "🏷️ 请输入新服务器的节点别名 (例如 hk-node-1):", b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_SSH_ALIAS" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			aliasVal := strings.TrimSpace(text)
			if aliasVal == "" || len(aliasVal) > 63 || strings.ContainsAny(aliasVal, " \t\n\r") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 别名无效（不能包含空格/换行，长度不超过 63 字节），请重新输入:")
				return
			}

			_, err := b.dbManager.GetNode(aliasVal)
			if err == nil {
				b.sendErrorReplyWithCancel(chatID, fromUID, fmt.Sprintf("❌ 节点别名 `%s` 已存在，请重新输入另一个别名:", aliasVal))
				return
			}

			b.userStatesMu.Lock()
			state.SSHNodeAlias = aliasVal
			state.UpdatedAt = time.Now()
			b.userStatesMu.Unlock()

			addr := b.gRPCServer.addr
			displayAddr := addr
			if strings.HasPrefix(addr, "127.0.0.1") || strings.HasPrefix(addr, "0.0.0.0") || strings.HasPrefix(addr, ":") {
				detectedIP := detectMasterIP()
				portPart := "50051"
				if strings.Contains(addr, ":") {
					parts := strings.Split(addr, ":")
					if len(parts) > 0 {
						portPart = parts[len(parts)-1]
					}
				}
				displayAddr = fmt.Sprintf("%s:%s", detectedIP, portPart)
			}

			masterKeyboard := tgbotapi.NewReplyKeyboard(
				tgbotapi.NewKeyboardButtonRow(
					tgbotapi.NewKeyboardButton(displayAddr),
					tgbotapi.NewKeyboardButton("❌ 取消操作"),
				),
			)
			masterKeyboard.ResizeKeyboard = true
			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SSH_MASTER_IP", "🌐 请确认或输入 Master 对外公网 IP (或域名) 供 Agent 连接:", masterKeyboard)
			return
		}

		if state.Step == "WAITING_FOR_SSH_MASTER_IP" {
			b.deleteMessage(chatID, msgID)

			masterAddrVal := strings.TrimSpace(text)
			if masterAddrVal == "" || strings.Contains(masterAddrVal, "YOUR_MASTER_PUBLIC_IP") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ IP 地址无效，请输入实际的可访问公网 IP/域名及端口:")
				return
			}

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			if state.PromptMsgID > 0 {
				b.deleteMessage(chatID, state.PromptMsgID)
			}
			for _, id := range state.UserMsgIDs {
				b.deleteMessage(chatID, id)
			}

			go b.startSSHDeployProcess(chatID, user, state, masterAddrVal)
			return
		}

		if state.Step == "WAITING_FOR_CUSTOM_INVITE_USES" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			uses, err := strconv.Atoi(text)
			if err != nil || uses <= 0 {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 激活次数无效，请输入正整数:")
				return
			}

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			b.clearConversationHistory(chatID, state)

			b.handleInvite(chatID, fromUID, uses)
			return
		}

		if state.Step == "WAITING_FOR_CF_TOKEN" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			apiTokenRegex := regexp.MustCompile(`您的 API 令牌\s*([A-Za-z0-9_-]+)`)
			accessKeyRegex := regexp.MustCompile(`访问密钥 ID\s*([A-Za-z0-9]+)`)
			secretKeyRegex := regexp.MustCompile(`秘密访问密钥\s*([A-Za-z0-9]+)`)
			endpointRegex := regexp.MustCompile(`S3 API 端点\s*(https://[A-Za-z0-9.-]+)`)

			apiTokenMatch := apiTokenRegex.FindStringSubmatch(text)
			accessKeyMatch := accessKeyRegex.FindStringSubmatch(text)
			secretKeyMatch := secretKeyRegex.FindStringSubmatch(text)
			endpointMatch := endpointRegex.FindStringSubmatch(text)

			if len(apiTokenMatch) > 1 {
				b.userStatesMu.Lock()
				state.CFToken = apiTokenMatch[1]
				if len(accessKeyMatch) > 1 {
					state.CFR2AccessKeyID = accessKeyMatch[1]
				}
				if len(secretKeyMatch) > 1 {
					state.CFR2SecretAccessKey = secretKeyMatch[1]
				}
				if len(endpointMatch) > 1 {
					state.CFR2Endpoint = endpointMatch[1]
				}
				b.userStatesMu.Unlock()

				msg := "✅ 成功提取 API Token！"
				if len(accessKeyMatch) > 1 && len(secretKeyMatch) > 1 {
					msg = "✅ 成功智能解析 API Token 及 R2 (S3) 对象存储凭证！"
				}
				msg += "\n接下来，请输入 Cloudflare Zone ID："

				b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_ZONE", msg, b.getCancelKeyboard())
				return
			}

			token := strings.TrimSpace(text)
			if token == "" || strings.Contains(token, " ") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ Token 格式错误或未识别到所需字段，请重新输入:")
				return
			}

			b.userStatesMu.Lock()
			state.CFToken = token
			b.userStatesMu.Unlock()
			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_ZONE", "☁️ 请输入 Cloudflare Zone ID (或输入 no 跳过):", b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_CF_ZONE" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			zone := strings.TrimSpace(text)
			if strings.ToLower(zone) == "no" {
				zone = ""
			} else if zone == "" || strings.Contains(zone, " ") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ Zone ID 格式错误，请重新输入或输入 'no' 跳过:")
				return
			}

			b.userStatesMu.Lock()
			state.CFZone = zone
			b.userStatesMu.Unlock()

			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_NAME", "☁️ 请输入此配置的备注名称 (例如：业务专用组A):", b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_CF_NAME" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			name := strings.TrimSpace(text)
			if name == "" {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 备注名称不能为空，请重新输入:")
				return
			}

			buf := make([]byte, 8)
			_, _ = rand.Read(buf)
			cfID := fmt.Sprintf("%X-%d", buf, time.Now().UnixNano())

			config := &types.CFConfig{
				ID:                cfID,
				Name:              name,
				APIToken:          state.CFToken,
				ZoneID:            state.CFZone,
				R2AccessKeyID:     state.CFR2AccessKeyID,
				R2SecretAccessKey: state.CFR2SecretAccessKey,
				R2Endpoint:        state.CFR2Endpoint,
				CreatedBy:         fromUID,
				CreatedAt:         time.Now(),
			}

			err := b.dbManager.SaveCFConfig(config)
			
			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			b.clearConversationHistory(chatID, state)

			if err != nil {
				b.replyWithMarkup(chatID, "❌ Cloudflare 配置保存失败: "+err.Error(), b.getMainMenuMarkup(user))
			} else {
				if b.useMock {
					log.Printf("[Bot Mock] REPLY: Cloudflare 配置保存成功！")
				} else {
					msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🎉 **Cloudflare API 凭证 '%s' 保存成功！**\n您可以在部署应用时选择使用此套配置。", name))
					msg.ReplyMarkup = b.getMainMenuMarkup(user)
					_, _ = b.api.Send(msg)
				}
			}
			return
		}

		if state.Step == "WAITING_FOR_CF_ALLOCATE_USER" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			uid, err := extractUIDFromButton(text)
			if err != nil || uid <= 0 {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 用户 UID 输入无效，请重新输入:")
				return
			}

			targetUser, err := b.dbManager.GetUser(uid)
			if err != nil || targetUser == nil {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 找不到该用户，请重试:")
				return
			}

			b.userStatesMu.Lock()
			state.TargetUID = uid // Reuse TargetUID to store target user
			b.userStatesMu.Unlock()

			cfs, err := b.dbManager.ListCFConfigs()
			if err != nil || len(cfs) == 0 {
				b.userStatesMu.Lock()
				delete(b.userStates, fromUID)
				b.userStatesMu.Unlock()
				b.clearConversationHistory(chatID, state)
				b.replyWithMarkup(chatID, "⚠️ 系统中当前没有任何 Cloudflare 配置，请先添加配置。", b.getMainMenuMarkup(user))
				return
			}

			var buttons [][]tgbotapi.KeyboardButton
			var row []tgbotapi.KeyboardButton
			for i, cf := range cfs {
				btnText := fmt.Sprintf("☁️ %s (%s)", cf.Name, cf.ID)
				row = append(row, tgbotapi.NewKeyboardButton(btnText))
				if (i+1)%2 == 0 {
					buttons = append(buttons, row)
					row = nil
				}
			}
			if len(row) > 0 {
				buttons = append(buttons, row)
			}
			buttons = append(buttons, []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("❌ 取消操作")})
			replyMarkup := tgbotapi.NewReplyKeyboard(buttons...)
			replyMarkup.ResizeKeyboard = true

			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_ALLOCATE_CHOICE", fmt.Sprintf("🔧 **请为用户 %s (%d) 选择要分配的 Cloudflare 配置:**", targetUser.Name, targetUser.UID), replyMarkup)
			return
		}

		if state.Step == "WAITING_FOR_CF_ALLOCATE_CHOICE" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			// Extract CF ID
			parts := strings.Split(text, "(")
			if len(parts) < 2 {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 选择格式无效，请点击下方键盘按钮:")
				return
			}
			cfID := strings.TrimSuffix(parts[len(parts)-1], ")")

			targetUID := state.TargetUID
			targetUser, err := b.dbManager.GetUser(targetUID)
			
			if err == nil && targetUser != nil {
				targetUser.AssignedCF = cfID
				err = b.dbManager.SaveUser(targetUser)
			}

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()
			b.clearConversationHistory(chatID, state)

			if err != nil {
				b.replyWithMarkup(chatID, "❌ 分配配置失败: "+err.Error(), b.getMainMenuMarkup(user))
			} else {
				msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🎉 **成功为用户分配 Cloudflare 配置！**\n该用户后续部署应用将默认使用这套网络配置。"))
				msg.ReplyMarkup = b.getMainMenuMarkup(user)
				_, _ = b.api.Send(msg)
			}
			return
		}

		if state.Step == "WAITING_FOR_FREEZE_UID" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			uid, err := extractUIDFromButton(text)
			if err != nil || uid <= 0 {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ UID 输入无效，请重新输入:")
				return
			}

			errDel := b.dbManager.DeleteUser(uid)

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			b.clearConversationHistory(chatID, state)

			if errDel != nil {
				b.reply(chatID, "❌ 冻结用户失败: "+errDel.Error())
			} else {
				if b.useMock {
					log.Printf("[Bot Mock] REPLY: 用户已冻结。")
				} else {
					msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("❌ **已成功冻结并注销该账户 (UID: `%d`)！**\n该用户已丧失所有 ChatOps 控制特权。", uid))
					msg.ReplyMarkup = b.getMainMenuMarkup(user)
					_, _ = b.api.Send(msg)
				}
			}
			return
		}

		if state.Step == "WAITING_FOR_ADMIN_UID" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			uid, err := extractUIDFromButton(text)
			if err != nil || uid <= 0 {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ UID 无效，请重新输入:")
				return
			}

			targetUser, errGet := b.dbManager.GetUser(uid)
			if errGet != nil {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 未找到该已绑定的子账户，请重新输入:")
				return
			}

			targetUser.Role = "admin"
			if targetUser.Permissions == nil {
				targetUser.Permissions = make(map[string]bool)
			}
			errSave := b.dbManager.SaveUser(targetUser)

			if errSave != nil {
				b.clearConversationHistory(chatID, state)
				b.userStatesMu.Lock()
				delete(b.userStates, fromUID)
				b.userStatesMu.Unlock()
				b.replyWithMarkup(chatID, "❌ 初始化管理员权限失败: "+errSave.Error(), b.getMainMenuMarkup(user))
				return
			}

			b.userStatesMu.Lock()
			state.Step = "WAITING_FOR_ADMIN_PERMS"
			state.TargetUID = uid // Reuse TargetUID to pass target user ID
			b.userStatesMu.Unlock()

			permMsg := fmt.Sprintf("👑 **正在为管理员 (UID: %d) 分配细粒度权限**\n请回复所需权限的数字编号（多个请用逗号分隔，例如 1,2,4）：\n" +
				"1. ☁️ 配置 Cloudflare\n" +
				"2. 🔧 分配 Cloudflare\n" +
				"3. 🔍 检查更新\n" +
				"4. 🔑 邀请新人\n" +
				"5. 👑 设置管理员", uid)
			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_ADMIN_PERMS", permMsg, b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_ADMIN_PERMS" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			targetUID := state.TargetUID
			targetUser, err := b.dbManager.GetUser(targetUID)
			if err != nil || targetUser == nil {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 找不到目标用户，请重试:")
				return
			}

			perms := strings.Split(text, ",")
			if targetUser.Permissions == nil {
				targetUser.Permissions = make(map[string]bool)
			}
			
			// Clear old
			for k := range targetUser.Permissions {
				delete(targetUser.Permissions, k)
			}

			for _, p := range perms {
				p = strings.TrimSpace(p)
				switch p {
				case "1":
					if b.hasPermission(user, types.PermCFConfig) {
						targetUser.Permissions[types.PermCFConfig] = true
					}
				case "2":
					if b.hasPermission(user, types.PermCFAllocate) {
						targetUser.Permissions[types.PermCFAllocate] = true
					}
				case "3":
					if b.hasPermission(user, types.PermCheckUpdate) {
						targetUser.Permissions[types.PermCheckUpdate] = true
					}
				case "4":
					if b.hasPermission(user, types.PermInvite) {
						targetUser.Permissions[types.PermInvite] = true
					}
				case "5":
					if b.hasPermission(user, types.PermSetAdmin) {
						targetUser.Permissions[types.PermSetAdmin] = true
					}
				}
			}

			errSave := b.dbManager.SaveUser(targetUser)

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()
			b.clearConversationHistory(chatID, state)

			if errSave != nil {
				b.replyWithMarkup(chatID, "❌ 权限保存失败: "+errSave.Error(), b.getMainMenuMarkup(user))
			} else {
				msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🎉 **成功为副管理员 (UID: %d) 分配权限！**", targetUID))
				msg.ReplyMarkup = b.getMainMenuMarkup(user)
				_, _ = b.api.Send(msg)
			}
			return
		}

		if state.Step == "WAITING_FOR_ADD_ADMIN_UID" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			uid, err := extractUIDFromButton(text)
			if err != nil || uid <= 0 {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ UID 无效，请重新输入:")
				return
			}

			targetUser, errGet := b.dbManager.GetUser(uid)
			if errGet == nil {
				if targetUser.Role == "master" || targetUser.Role == "admin" {
					b.sendErrorReplyWithCancel(chatID, fromUID, "⚠️ 该用户已是管理员，请重新输入:")
					return
				}
				targetUser.Role = "admin"
			} else {
				targetUser = &types.User{
					UID:      uid,
					Role:     "admin",
					JoinedAt: time.Now(),
				}
			}

			if targetUser.Permissions == nil {
				targetUser.Permissions = make(map[string]bool)
			}
			errSave := b.dbManager.SaveUser(targetUser)

			if errSave != nil {
				b.clearConversationHistory(chatID, state)
				b.userStatesMu.Lock()
				delete(b.userStates, fromUID)
				b.userStatesMu.Unlock()
				b.replyWithMarkup(chatID, "❌ 初始化管理员权限失败: "+errSave.Error(), b.getMainMenuMarkup(user))
				return
			}

			b.userStatesMu.Lock()
			state.Step = "WAITING_FOR_ADMIN_PERMS"
			state.TargetUID = uid // Reuse TargetUID to pass target user ID
			b.userStatesMu.Unlock()

			permMsg := fmt.Sprintf("👑 **正在为直接添加的管理员 (UID: %d) 分配细粒度权限**\n请回复所需权限的数字编号（多个请用逗号分隔，例如 1,2,4）：\n" +
				"1. ☁️ 配置 Cloudflare\n" +
				"2. 🔧 分配 Cloudflare\n" +
				"3. 🔍 检查更新\n" +
				"4. 🔑 邀请新人\n" +
				"5. 👑 设置管理员", uid)
			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_ADMIN_PERMS", permMsg, b.getCancelKeyboard())
			return
		}
	}

	parts := strings.Fields(text)
	cmd := ""
	if len(parts) > 0 {
		cmd = parts[0]
	}

	// 3. Command & Text button routing
	switch {
	case cmd == "/start" || cmd == "/menu":
		b.showMainMenu(chatID, user)

	case cmd == "/deploy":
		if len(parts) < 3 {
			b.reply(chatID, "❌ 参数不足。用法: `/deploy <git_url> <node_alias>`")
			return
		}
		b.handleDeploy(chatID, fromUID, parts[1], parts[2], "", "none", false, msgID)

	case text == "🚀 部署应用":
		b.deleteMessage(chatID, msgID)
		b.showNodeSelectionMenu(chatID, fromUID)

	case text == "🔑 生成邀请码" || cmd == "/invite":
		if !b.hasPermission(user, types.PermInvite) {
			b.reply(chatID, "❌ 权限不足。仅授权管理员可生成邀请码。")
			return
		}

		if text == "🔑 生成邀请码" || cmd == "/invite" {
			if text == "🔑 生成邀请码" {
				b.deleteMessage(chatID, msgID)
			}

			b.userStatesMu.Lock()
			b.userStates[fromUID] = &UserConversationState{
				Step:      "WAITING_FOR_CUSTOM_INVITE_USES",
				UpdatedAt: time.Now(),
			}
			b.userStatesMu.Unlock()

			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CUSTOM_INVITE_USES", "🔑 请发送激活次数:", b.getCancelKeyboard())
		}

	case text == "🖥️ 节点状态":
		b.deleteMessage(chatID, msgID)

		nodes, err := b.dbManager.ListNodes()
		if err != nil {
			b.reply(chatID, "❌ 数据库查询节点失败。")
			return
		}

		var sb strings.Builder
		sb.WriteString("🖥️ **GOPASS 集群节点实时状态**\n")
		sb.WriteString("————————————————————\n")
		for _, n := range nodes {
			status := "🔴 离线"
			if n.Connected {
				status = "🟢 在线"
			}
			sb.WriteString(fmt.Sprintf("📍 **节点别名**: `%s` (%s)\n", n.Alias, status))
			if n.Connected {
				sb.WriteString(fmt.Sprintf("   - CPU 使用率: `%.1f%%`\n", n.Hardware.CPUUsage))
				sb.WriteString(fmt.Sprintf("   - 内存使用率: `%.1f%%`\n", n.Hardware.MemoryUsage))
				if n.IP != "" {
					sb.WriteString(fmt.Sprintf("   - 节点 IP: `%s`\n", n.IP))
				}

				// List Deployments
				deps, _ := b.dbManager.ListDeploymentsByNode(n.Alias)
				if len(deps) > 0 {
					sb.WriteString("   - 📦 部署的项目:\n")
					for _, d := range deps {
						sb.WriteString(fmt.Sprintf("       • `%s` (域名: %s:%d, 状态: %s)\n", d.ProjectName, d.Domain, d.Port, d.Status))
					}
				} else {
					sb.WriteString("   - 📦 部署的项目: 无\n")
				}
			}
			sb.WriteString("\n")
		}

		if !b.useMock {
			msgCfg := tgbotapi.NewMessage(chatID, sb.String())
			msgCfg.ParseMode = "Markdown"
			
			if user.Role == "master" {
				var buttons [][]tgbotapi.KeyboardButton
				buttons = append(buttons, []tgbotapi.KeyboardButton{
					tgbotapi.NewKeyboardButton("➕ 添加服务器"),
					tgbotapi.NewKeyboardButton("⬅️ 返回主菜单"),
				})
				replyMarkup := tgbotapi.NewReplyKeyboard(buttons...)
				replyMarkup.ResizeKeyboard = true
				msgCfg.ReplyMarkup = replyMarkup
			} else {
				msgCfg.ReplyMarkup = b.getMainMenuMarkup(user)
			}
			_, _ = b.api.Send(msgCfg)
		} else {
			log.Printf("[Bot Mock] Edited status message")
		}

	case text == "➕ 添加服务器":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。仅最高管理员主账户可添加服务器。")
			return
		}

		b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SSH_HOST", "🖥️ 请输入目标服务器的 IP 地址 (例如 192.168.1.100):", b.getCancelKeyboard())
		return

	case text == "👥 账户管理":
		b.deleteMessage(chatID, msgID)
		if !b.hasPermission(user, types.PermSetAdmin) {
			b.reply(chatID, "❌ 权限不足。仅授权管理员可管理账户。")
			return
		}
		b.showAccountMenu(chatID, user)

	case text == "📜 查看子账户":
		b.deleteMessage(chatID, msgID)
		if !b.hasPermission(user, types.PermSetAdmin) {
			b.reply(chatID, "❌ 权限不足。")
			return
		}

		users, err := b.dbManager.ListUsers()
		if err != nil {
			b.reply(chatID, "❌ 数据库查询用户失败。")
			return
		}

		var sb strings.Builder
		sb.WriteString("👥 **GOPASS 集群注册账户列表**\n")
		sb.WriteString("————————————————————\n")
		for _, u := range users {
			roleName := "普通成员"
			if u.Role == "master" {
				roleName = "最高管理员 👑"
			} else if u.Role == "admin" {
				roleName = "副管理员 👤"
			}
			nameStr := u.Name
			if nameStr == "" {
				nameStr = "未知"
			}
			sb.WriteString(fmt.Sprintf("👤 **%s** (`%d`)\n   - 角色: `%s`\n   - 加入时间: %s\n\n", nameStr, u.UID, roleName, u.JoinedAt.Format("2006-01-02 15:04")))
		}

		msgCfg := tgbotapi.NewMessage(chatID, sb.String())
		msgCfg.ParseMode = "Markdown"
		msgCfg.ReplyMarkup = b.getMainMenuMarkup(user)
		_, _ = b.api.Send(msgCfg)

	case text == "❌ 冻结子账户":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。")
			return
		}
		b.showUserSelectionKeyboard(chatID, fromUID, "WAITING_FOR_FREEZE_UID", "❌ **请在下方选择或手动输入要注销并冻结的账户 UID：**")

	case text == "👑 设为管理员":
		b.deleteMessage(chatID, msgID)
		if !b.hasPermission(user, types.PermSetAdmin) {
			b.reply(chatID, "❌ 权限不足。")
			return
		}
		b.showUserSelectionKeyboard(chatID, fromUID, "WAITING_FOR_ADMIN_UID", "👑 **请在下方选择或手动输入要提升为副管理员的子账户 UID：**")

	case text == "➕ 增添管理员":
		b.deleteMessage(chatID, msgID)
		if !b.hasPermission(user, types.PermSetAdmin) {
			b.reply(chatID, "❌ 权限不足。")
			return
		}
		b.userStatesMu.Lock()
		b.userStates[fromUID] = &UserConversationState{
			Step:        "WAITING_FOR_ADD_ADMIN_UID",
			UpdatedAt:   time.Now(),
		}
		b.userStatesMu.Unlock()

		promptText := "👑 **[增添管理员] 请输入要直接增添为副管理员的 Telegram 用户 UID:**\n\n发送 `/cancel` 可以退出当前会话。"
		promptMsgID := b.showCancelKeyboard(chatID, promptText)

		b.userStatesMu.Lock()
		state, hasState := b.userStates[fromUID]
		if hasState {
			state.PromptMsgID = promptMsgID
		}
		b.userStatesMu.Unlock()

	case text == "⚙️ 更多功能":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" && !b.hasPermission(user, types.PermCFConfig) && !b.hasPermission(user, types.PermCFAllocate) && !b.hasPermission(user, types.PermCheckUpdate) {
			b.replyWithMarkup(chatID, "❌ 权限不足。仅授权管理员可访问更多功能板块。", b.getMainMenuMarkup(user))
			return
		}
		b.showMoreFunctionsMenu(chatID, user)

	case text == "☁️ 配置 Cloudflare":
		b.deleteMessage(chatID, msgID)
		if !b.hasPermission(user, types.PermCFConfig) {
			b.reply(chatID, "❌ 权限不足。")
			return
		}

		b.userStatesMu.Lock()
		b.userStates[fromUID] = &UserConversationState{
			Step:      "WAITING_FOR_CF_TOKEN",
			UpdatedAt: time.Now(),
		}
		b.userStatesMu.Unlock()

		b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_TOKEN", "☁️ 请输入 Cloudflare API Token:", b.getCancelKeyboard())

	case text == "🔧 分配 Cloudflare":
		b.deleteMessage(chatID, msgID)
		if !b.hasPermission(user, types.PermCFAllocate) {
			b.reply(chatID, "❌ 权限不足。")
			return
		}
		b.showUserSelectionKeyboard(chatID, fromUID, "WAITING_FOR_CF_ALLOCATE_USER", "🔧 **请选择要分配 Cloudflare 配置的用户 UID:**")

	case text == "📜 查看 Master 日志":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。")
			return
		}

		data, err := os.ReadFile("gopass-master.log")
		var content string
		if err != nil {
			content = "⚠️ 暂无系统日志记录或无法读取日志文件。"
		} else {
			lines := strings.Split(string(data), "\n")
			start := len(lines) - 20
			if start < 0 {
				start = 0
			}
			content = "📜 **GOPASS MASTER 最新 20 行日志记录**：\n————————————————————\n```text\n" + strings.Join(lines[start:], "\n") + "\n```"
		}

		msgCfg := tgbotapi.NewMessage(chatID, content)
		msgCfg.ParseMode = "Markdown"
		msgCfg.ReplyMarkup = b.getMainMenuMarkup(user)
		_, _ = b.api.Send(msgCfg)

	case text == "🔍 检查更新":
		b.deleteMessage(chatID, msgID)
		if !b.hasPermission(user, types.PermCheckUpdate) {
			b.reply(chatID, "❌ 权限不足。")
			return
		}

		b.updateWizardPrompt(chatID, fromUID, "CHECKING_UPDATE", "🔄 正在从 GitHub 检查最新版本...", nil)

		release, err := FetchLatestRelease(b.githubToken)
		if err != nil {
			if errors.Is(err, ErrNoReleaseFound) {
				msgText := fmt.Sprintf("✨ **当前已是最新版本 (%s)！**\n\nGitHub 上未发现任何发布版本。", types.CurrentVersion)
				b.updateWizardPrompt(chatID, fromUID, "IDLE", msgText, b.getMainMenuMarkup(user))
				return
			}
			b.updateWizardPrompt(chatID, fromUID, "IDLE", "❌ 检查更新失败：" + err.Error(), b.getMainMenuMarkup(user))
			return
		}

		if !IsNewerVersion(types.CurrentVersion, release.TagName) {
			msgText := fmt.Sprintf("✨ **当前已是最新版本 (%s)！**\n\nGitHub 最新版本: `%s`", types.CurrentVersion, release.TagName)
			b.updateWizardPrompt(chatID, fromUID, "IDLE", msgText, b.getMainMenuMarkup(user))
			return
		}

		// Found newer version!
		msgText := fmt.Sprintf("🔍 **发现新版本**: `%s` (当前: `%s`)\n————————————————————\n**更新日志**:\n%s\n\n是否立即对 Master 节点执行自动升级？\n请手动输入 **yes** 或 **no** 进行确认：", release.TagName, types.CurrentVersion, release.Body)
		
		var buttons [][]tgbotapi.KeyboardButton
		row1 := []tgbotapi.KeyboardButton{
			tgbotapi.NewKeyboardButton("yes"),
			tgbotapi.NewKeyboardButton("no"),
		}
		buttons = append(buttons, row1)
		replyMarkup := tgbotapi.NewReplyKeyboard(buttons...)
		replyMarkup.ResizeKeyboard = true

		b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_MASTER_UPDATE_CONFIRM", msgText, replyMarkup)

	case text == "🖥️ 升级 Agent":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。")
			return
		}

		nodes, err := b.dbManager.ListNodes()
		if err != nil {
			b.reply(chatID, "❌ 数据库查询节点失败。")
			return
		}

		var activeNodes []*types.ServerNode
		for _, n := range nodes {
			if n.Connected {
				activeNodes = append(activeNodes, n)
			}
		}

		if len(activeNodes) == 0 {
			b.updateWizardPrompt(chatID, fromUID, "IDLE", "⚠️ 当前没有在线的 Agent 节点！无法进行升级。", b.getMainMenuMarkup(user))
			return
		}

		var replyButtons [][]tgbotapi.KeyboardButton
		var currentRow []tgbotapi.KeyboardButton
		for i, n := range activeNodes {
			btn := tgbotapi.NewKeyboardButton(fmt.Sprintf("🖥️ 升级节点 %s", n.Alias))
			currentRow = append(currentRow, btn)
			if (i+1)%2 == 0 {
				replyButtons = append(replyButtons, currentRow)
				currentRow = nil
			}
		}
		if len(currentRow) > 0 {
			replyButtons = append(replyButtons, currentRow)
		}
		replyButtons = append(replyButtons, []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("⬅️ 返回主菜单")})

		replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
		replyMarkup.ResizeKeyboard = true

		b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_AGENT_UPGRADE_SELECTION", "🖥️ 请选择要升级的被控端 Agent 节点:", replyMarkup)

	case strings.HasPrefix(text, "🖥️ 升级节点 "):
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。")
			return
		}

		nodeAlias := strings.TrimPrefix(text, "🖥️ 升级节点 ")
		
		release, err := FetchLatestRelease(b.githubToken)
		if err != nil {
			if errors.Is(err, ErrNoReleaseFound) {
				b.updateWizardPrompt(chatID, fromUID, "IDLE", "❌ 无法获取最新 Release：GitHub 未发布任何 Release 版本，请先发布 Release 后再试。", b.getMainMenuMarkup(user))
				return
			}
			b.updateWizardPrompt(chatID, fromUID, "IDLE", "❌ 无法获取最新 Release: " + err.Error(), b.getMainMenuMarkup(user))
			return
		}

		templateURL := fmt.Sprintf("https://github.com/AltProto-Studio/ChatOps/releases/download/%s/gopass-agent-{{OS}}-{{ARCH}}{{EXT}}", release.TagName)

		b.updateWizardPrompt(chatID, fromUID, "UPGRADING_AGENT", fmt.Sprintf("📡 正在向 Agent '%s' 下发升级指令 (版本: %s)...", nodeAlias, release.TagName), nil)

		success := b.gRPCServer.SendUpdateTask(nodeAlias, templateURL, b.githubToken, release.TagName)
		if !success {
			b.updateWizardPrompt(chatID, fromUID, "IDLE", fmt.Sprintf("❌ 向 Agent '%s' 发送升级指令失败，节点可能已下线。", nodeAlias), b.getMainMenuMarkup(user))
			return
		}

		b.updateWizardPrompt(chatID, fromUID, "IDLE", fmt.Sprintf("✅ 已成功向 Agent '%s' 下发升级指令。该节点将自动下载新版本并重连。请稍后在 '🖥️ 节点状态' 中查看升级结果。", nodeAlias), b.getMainMenuMarkup(user))

	case cmd == "/join":
		if len(parts) < 2 {
			b.reply(chatID, "❌ 请提供激活码。用法: `/join <激活码>`")
			return
		}
		b.handleJoin(chatID, fromUID, parts[1], msgID, displayName)

	default:
		// Fallback for random hand-typed inputs: send main dashboard Reply Keyboard
		b.showMainMenu(chatID, user)
	}
}

func (b *Bot) getMainMenuMarkup(user *types.User) tgbotapi.ReplyKeyboardMarkup {
	var replyButtons [][]tgbotapi.KeyboardButton

	// Default row for everyone
	row1 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("🚀 部署应用"), tgbotapi.NewKeyboardButton("🖥️ 节点状态")}

	var row2 []tgbotapi.KeyboardButton
	if b.hasPermission(user, types.PermInvite) {
		row2 = append(row2, tgbotapi.NewKeyboardButton("🔑 生成邀请码"))
	}
	if b.hasPermission(user, types.PermSetAdmin) {
		row2 = append(row2, tgbotapi.NewKeyboardButton("👥 账户管理"))
	}

	var row3 []tgbotapi.KeyboardButton
	if user != nil && (user.Role == "master" || user.Role == "admin") {
		// As long as they are admin, they might have some 'more functions'
		row3 = append(row3, tgbotapi.NewKeyboardButton("⚙️ 更多功能"))
	}
	row3 = append(row3, tgbotapi.NewKeyboardButton("❌ 取消操作"))

	replyButtons = append(replyButtons, row1)
	if len(row2) > 0 {
		replyButtons = append(replyButtons, row2)
	}
	replyButtons = append(replyButtons, row3)

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true
	return replyMarkup
}

// showMainMenu renders the TTY ReplyKeyboardMarkup below the text input bar
func (b *Bot) showMainMenu(chatID int64, user *types.User) {
	text := "🤖 主菜单"

	replyMarkup := b.getMainMenuMarkup(user)

	if b.useMock {
		log.Printf("[Bot Mock] REPLY Main Menu to Chat %d with Reply Keyboard", chatID)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = replyMarkup
	_, _ = b.api.Send(msg)
}

func (b *Bot) showNodeSelectionMenu(chatID int64, fromUID int64) {
	nodes, err := b.dbManager.ListNodes()
	if err != nil {
		b.reply(chatID, "❌ 数据库查询节点失败。")
		return
	}

	var activeNodes []*types.ServerNode
	for _, n := range nodes {
		if n.Connected {
			activeNodes = append(activeNodes, n)
		}
	}

	if len(activeNodes) == 0 {
		b.reply(chatID, "⚠️ 当前没有在线的 Agent 节点！")
		return
	}

	var replyButtons [][]tgbotapi.KeyboardButton
	var currentRow []tgbotapi.KeyboardButton
	for i, n := range activeNodes {
		btn := tgbotapi.NewKeyboardButton(fmt.Sprintf("🖥️ %s", n.Alias))
		currentRow = append(currentRow, btn)
		if (i+1)%2 == 0 {
			replyButtons = append(replyButtons, currentRow)
			currentRow = nil
		}
	}
	if len(currentRow) > 0 {
		replyButtons = append(replyButtons, currentRow)
	}
	replyButtons = append(replyButtons, []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("⬅️ 返回主菜单")})

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	text := "🚀 请选择目标部署节点:"
	b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_NODE_SELECTION", text, replyMarkup)
}

func (b *Bot) showDomainTypeMenu(chatID int64, fromUID int64) {
	var replyButtons [][]tgbotapi.KeyboardButton
	row1 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("🌐 直接使用节点公网 IP"),
		tgbotapi.NewKeyboardButton("☁️ 绑定 Cloudflare 域名 (推荐)"),
	}
	row2 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("❌ 取消操作"),
	}
	replyButtons = append(replyButtons, row1, row2)

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	text := "🌐 请选择部署访问方式:"
	b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_DOMAIN_TYPE", text, replyMarkup)
}

func (b *Bot) showCFDNSTypeMenu(chatID int64, fromUID int64) {
	var replyButtons [][]tgbotapi.KeyboardButton
	row1 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("☁️ 开启 CF 解析 + CDN Proxy"),
		tgbotapi.NewKeyboardButton("☁️ 仅解析 DNS"),
	}
	row2 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("☁️ 不使用 CF 解析"),
	}
	replyButtons = append(replyButtons, row1, row2)

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	text := "☁️ 是否开启 Cloudflare 解析:"
	b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_DNS_TYPE", text, replyMarkup)
}

func (b *Bot) showSSLTypeMenu(chatID int64, fromUID int64) {
	var replyButtons [][]tgbotapi.KeyboardButton
	row1 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("🔒 自动 SSL (Caddy 自动申请)"),
		tgbotapi.NewKeyboardButton("🔓 仅 HTTP 部署"),
	}
	row2 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("❌ 取消操作"),
	}
	replyButtons = append(replyButtons, row1, row2)

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	text := "🔒 请选择 SSL 证书模式:"
	b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_SSL_TYPE", text, replyMarkup)
}

func (b *Bot) showAccountMenu(chatID int64, user *types.User) {
	var replyButtons [][]tgbotapi.KeyboardButton
	row1 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("📜 查看子账户"),
		tgbotapi.NewKeyboardButton("➕ 增添管理员"),
	}
	row2 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("👑 设为管理员"),
		tgbotapi.NewKeyboardButton("❌ 冻结子账户"),
	}
	row3 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("⬅️ 返回主菜单"),
	}
	replyButtons = append(replyButtons, row1, row2, row3)

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	text := "👥 账户管理"
	if b.useMock {
		log.Printf("[Bot Mock] Account menu sent to Chat %d", chatID)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = replyMarkup
	_, _ = b.api.Send(msg)
}

func (b *Bot) showMoreFunctionsMenu(chatID int64, user *types.User) {
	var replyButtons [][]tgbotapi.KeyboardButton

	var row1 []tgbotapi.KeyboardButton
	if b.hasPermission(user, types.PermCFConfig) {
		row1 = append(row1, tgbotapi.NewKeyboardButton("☁️ 配置 Cloudflare"))
	}
	if b.hasPermission(user, types.PermCFAllocate) {
		row1 = append(row1, tgbotapi.NewKeyboardButton("🔧 分配 Cloudflare"))
	}

	var row2 []tgbotapi.KeyboardButton
	if user != nil && user.Role == "master" {
		row2 = append(row2, tgbotapi.NewKeyboardButton("📜 查看 Master 日志"))
	}
	if b.hasPermission(user, types.PermCheckUpdate) {
		row2 = append(row2, tgbotapi.NewKeyboardButton("🔍 检查更新"))
	}

	if len(row1) > 0 {
		replyButtons = append(replyButtons, row1)
	}
	if len(row2) > 0 {
		replyButtons = append(replyButtons, row2)
	}
	replyButtons = append(replyButtons, []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("⬅️ 返回主菜单")})
	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	text := "⚙️ 更多功能"
	if b.useMock {
		log.Printf("[Bot Mock] More functions menu sent to Chat %d", chatID)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = replyMarkup
	_, _ = b.api.Send(msg)
}

func (b *Bot) showUserSelectionKeyboard(chatID int64, fromUID int64, step string, title string) {
	users, err := b.dbManager.ListUsers()
	if err != nil {
		b.reply(chatID, "❌ 数据库查询用户失败。")
		return
	}

	var targets []*types.User
	for _, u := range users {
		if u.UID != fromUID && u.Role == "sub" {
			targets = append(targets, u)
		}
	}

	if len(targets) == 0 {
		b.reply(chatID, "⚠️ 当前没有可以操作的普通子账户。")
		return
	}

	var replyButtons [][]tgbotapi.KeyboardButton
	var currentRow []tgbotapi.KeyboardButton
	for i, u := range targets {
		btnLabel := fmt.Sprintf("%d", u.UID)
		if u.Name != "" {
			btnLabel = fmt.Sprintf("%s (%d)", u.Name, u.UID)
		}
		btn := tgbotapi.NewKeyboardButton(btnLabel)
		currentRow = append(currentRow, btn)
		if (i+1)%2 == 0 {
			replyButtons = append(replyButtons, currentRow)
			currentRow = nil
		}
	}
	if len(currentRow) > 0 {
		replyButtons = append(replyButtons, currentRow)
	}
	replyButtons = append(replyButtons, []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("❌ 取消操作")})

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	if b.useMock {
		log.Printf("[Bot Mock] User selection keyboard sent for %s", step)
		return
	}

	msg := tgbotapi.NewMessage(chatID, title)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = replyMarkup
	sent, err := b.api.Send(msg)
	if err == nil {
		b.userStatesMu.Lock()
		b.userStates[fromUID] = &UserConversationState{
			Step:        step,
			PromptMsgID: sent.MessageID,
			UpdatedAt:   time.Now(),
		}
		b.userStatesMu.Unlock()
	}
}

func (b *Bot) getCancelKeyboard() tgbotapi.ReplyKeyboardMarkup {
	var replyButtons [][]tgbotapi.KeyboardButton
	row := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("❌ 取消操作")}
	replyButtons = append(replyButtons, row)
	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true
	return replyMarkup
}

func (b *Bot) showCancelKeyboard(chatID int64, text string) int {
	var replyButtons [][]tgbotapi.KeyboardButton
	row := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("❌ 取消操作")}
	replyButtons = append(replyButtons, row)

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	if b.useMock {
		log.Printf("[Bot Mock] Cancel keyboard sent to Chat %d: %s", chatID, text)
		return 0
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = replyMarkup
	sent, err := b.api.Send(msg)
	if err != nil {
		return 0
	}
	return sent.MessageID
}

// updateWizardPrompt updates the current wizard step prompt, deleting the old prompt to maintain a single-bubble view.
func (b *Bot) updateWizardPrompt(chatID int64, fromUID int64, step string, text string, replyMarkup interface{}) {
	b.userStatesMu.Lock()
	state, hasState := b.userStates[fromUID]
	if !hasState || state == nil {
		state = &UserConversationState{
			Step:      step,
			UpdatedAt: time.Now(),
		}
		b.userStates[fromUID] = state
	} else {
		state.Step = step
		state.UpdatedAt = time.Now()
	}
	oldPromptID := state.PromptMsgID
	b.userStatesMu.Unlock()

	// Delete old prompt message immediately before sending new one, if it exists
	if oldPromptID > 0 {
		b.deleteMessage(chatID, oldPromptID)
	}

	if b.useMock {
		log.Printf("[Bot Mock] WIZARD STEP %s -> Chat %d: %s", step, chatID, text)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if replyMarkup != nil {
		msg.ReplyMarkup = replyMarkup
	}
	sent, err := b.api.Send(msg)
	if err == nil {
		b.userStatesMu.Lock()
		if state, has := b.userStates[fromUID]; has && state != nil {
			state.PromptMsgID = sent.MessageID
		}
		b.userStatesMu.Unlock()
	}
}

// HandleCallbackQuery handles click events on Inline Keyboards
func (b *Bot) HandleCallbackQuery(cb *tgbotapi.CallbackQuery) {
	if cb == nil {
		return
	}

	fromUID := cb.From.ID
	chatID := cb.Message.Chat.ID
	messageID := cb.Message.MessageID

	// Answer callback to stop loading spinner in Telegram client
	answer := tgbotapi.NewCallback(cb.ID, "")
	defer func() {
		if !b.useMock {
			_, _ = b.api.Request(answer)
		}
	}()

	user, err := b.dbManager.GetUser(fromUID)
	if err != nil || user == nil {
		answer.Text = "❌ 权限不足，请先加入集群。"
		answer.ShowAlert = true
		return
	}

	data := cb.Data
	log.Printf("[Bot] Callback received from UID %d: %s", fromUID, data)

	switch {
	case data == "menu_back":
		b.deleteMessage(chatID, messageID)

	case data == "menu_cancel":
		b.userStatesMu.Lock()
		state, hasState := b.userStates[fromUID]
		if hasState {
			delete(b.userStates, fromUID)
		}
		b.userStatesMu.Unlock()

		b.deleteMessage(chatID, messageID)
		if hasState && state != nil {
			for _, id := range state.UserMsgIDs {
				b.deleteMessage(chatID, id)
			}
		}
		b.reply(chatID, "❌ 会话操作已取消。")
		b.showMainMenu(chatID, user)

	case data == "menu_cancel_and_back":
		b.userStatesMu.Lock()
		state, hasState := b.userStates[fromUID]
		if hasState {
			delete(b.userStates, fromUID)
		}
		b.userStatesMu.Unlock()

		b.deleteMessage(chatID, messageID)
		if hasState && state != nil {
			for _, id := range state.UserMsgIDs {
				b.deleteMessage(chatID, id)
			}
		}
		b.showMainMenu(chatID, user)

	default:
		answer.Text = "⚠️ 控制面板已集成在输入框下方键盘中，请直接点击下方按钮操作。"
		answer.ShowAlert = true
		b.deleteMessage(chatID, messageID)
	}
}

func (b *Bot) handleInvite(chatID int64, creatorUID int64, maxUses int) {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	activationCode := fmt.Sprintf("%X-%d", buf, time.Now().UnixNano())

	tok := &types.Token{
		Hash:      activationCode,
		MaxUses:   maxUses,
		UsedCount: 0,
		CreatedBy: creatorUID,
		CreatedAt: time.Now(),
	}

	if err := b.dbManager.SaveToken(tok); err != nil {
		user, _ := b.dbManager.GetUser(creatorUID)
		b.replyWithMarkup(chatID, "❌ 数据库保存激活码失败: "+err.Error(), b.getMainMenuMarkup(user))
		return
	}

	user, _ := b.dbManager.GetUser(creatorUID)
	if b.useMock {
		log.Printf("[Bot Mock] REPLY to Chat %d: 🔑 激活码生成成功！", chatID)
		return
	}

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🔑 **激活码生成成功！**\n激活码: `%s`\n最大可用次数: %d\n请提供给子账户运行: `/join %s`", activationCode, maxUses, activationCode))
	msg.ParseMode = "Markdown"
	if user != nil {
		msg.ReplyMarkup = b.getMainMenuMarkup(user)
	}
	_, _ = b.api.Send(msg)
}

func (b *Bot) handleJoin(chatID int64, userUID int64, code string, userMsgID int, displayName string) {
	code = strings.ToUpper(strings.TrimSpace(code))
	err := b.dbManager.UseToken(code, userUID)
	if err != nil {
		if errors.Is(err, db.ErrTokenInvalid) {
			b.reply(chatID, "❌ 激活码无效或不存在。")
		} else if errors.Is(err, db.ErrTokenMaxReached) {
			b.reply(chatID, "❌ 激活码使用次数已达上限。")
		} else {
			b.reply(chatID, "❌ 激活失败: "+err.Error())
		}
		return
	}

	// Update the user's display name after successful join
	user, _ := b.dbManager.GetUser(userUID)
	if user != nil && displayName != "" {
		user.Name = displayName
		_ = b.dbManager.SaveUser(user)
	}

	if b.useMock {
		log.Printf("[Bot Mock] REPLY to Chat %d: 🎉 绑定子账户成功！", chatID)
		return
	}

	msg := tgbotapi.NewMessage(chatID, "🎉 **绑定子账户成功！**\n您现在已获得 gopass 集群普通操作权限。")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = b.getMainMenuMarkup(user)

	sent, err := b.api.Send(msg)
	if err == nil {
		go func(replyID, uMsgID int) {
			time.Sleep(5 * time.Second)
			b.deleteMessage(chatID, replyID)
			if uMsgID > 0 {
				b.deleteMessage(chatID, uMsgID)
			}
		}(sent.MessageID, userMsgID)
	}
}

func (b *Bot) handleDeploy(chatID int64, userUID int64, gitURL, nodeAlias, domain, cfDNSType string, useSSL bool, userMsgID int) {
	node, err := b.dbManager.GetNode(nodeAlias)
	if err != nil || !node.Connected {
		b.reply(chatID, fmt.Sprintf("❌ 节点 '%s' 处于离线状态或未注册，部署任务取消。", nodeAlias))
		return
	}

	projectName := parseGitProject(gitURL)
	routingDomain := domain
	if routingDomain == "" {
		routingDomain = fmt.Sprintf("%s.chatops.io", projectName)
	}

	user, _ := b.dbManager.GetUser(userUID)
	msgID := b.sendProgressMsgWithMenu(chatID, "📥 正在配置并初始化部署任务...", user)

	taskID := fmt.Sprintf("task-%d", time.Now().UnixNano())
	session := &DeploymentSession{
		ChatID:      chatID,
		MessageID:   msgID,
		NodeAlias:   nodeAlias,
		ProjectName: projectName,
		Domain:      routingDomain,
		LastUpdated: time.Now(),
		UserMsgID:   userMsgID,
		UseSSL:      useSSL,
		CFDNSType:   cfDNSType,
		OwnerUID:    userUID,
	}

	b.sessionsMu.Lock()
	b.sessions[taskID] = session
	b.sessionsMu.Unlock()

	job := &DeployJob{
		TaskID:        taskID,
		GitURL:        gitURL,
		ProjectName:   projectName,
		RoutingDomain: routingDomain,
		RoutingPort:   8080,
		Env:           map[string]string{"GOPASS_DEPLOY": "true"},
		ChatID:        chatID,
		MessageID:     msgID,
		CreatedAt:     time.Now(),
		ExecuteFunc: func(j *DeployJob) error {
			task := &pb.DeployTask{
				TaskId:        j.TaskID,
				GitUrl:        j.GitURL,
				ProjectName:   j.ProjectName,
				RoutingDomain: j.RoutingDomain,
				RoutingPort:   int32(j.RoutingPort),
				Env:           j.Env,
				UseSsl:        useSSL,
			}
			log.Printf("[Bot] Dispatching task %s to Agent %s", j.TaskID, nodeAlias)
			return b.gRPCServer.Deploy(nodeAlias, task)
		},
	}

	position, isFirst := b.qc.GetQueue(nodeAlias).Push(job)
	if !isFirst {
		b.editMessageDirect(chatID, msgID, fmt.Sprintf("⚠️ **当前节点正在执行其他构建任务**\n您的任务已安全进入排队（排队位置：#%d）。", position))
		return
	}

	errOverload := CheckResourceOverload(node.Hardware.CPUUsage, node.Hardware.MemoryUsage)
	if errOverload != nil {
		b.editMessageDirect(chatID, msgID, fmt.Sprintf("⚠️ **目标节点负载过高，部署挂起中**\n节点 [%s] CPU: %.1f%%, 内存: %.1f%%\n任务已挂起进入排队，等待节点降温后自动唤醒...", nodeAlias, node.Hardware.CPUUsage, node.Hardware.MemoryUsage))
		return
	}

	job.Dispatched = true
	b.editMessageDirect(chatID, msgID, "📥 **正在拉取 Git 代码...**")
	go func() {
		if err := job.ExecuteFunc(job); err != nil {
			b.onTaskProgress(&pb.TaskProgress{
				TaskId:  job.TaskID,
				State:   "FAILED",
				LogLine: "gRPC dispatch failed: " + err.Error(),
			})
		}
	}()
}

func (b *Bot) onTaskProgress(p *pb.TaskProgress) {
	b.sessionsMu.Lock()
	session, exists := b.sessions[p.TaskId]
	b.sessionsMu.Unlock()

	if !exists {
		return
	}

	session.LogLines = append(session.LogLines, p.LogLine)

	var text string
	switch p.State {
	case "CLONING":
		text = "📥 **正在拉取 Git 代码...**\n" + p.LogLine
	case "BUILDING":
		text = "🛠️ **正在构建容器镜像 (Railpack)...**\n" + p.LogLine
	case "ROUTING":
		text = "🌐 **正在配置反向代理与端口调度...**\n" + p.LogLine
	case "DEPLOY_SUCCESS":
		var finalState struct {
			Domain string `json:"domain"`
			Port   int    `json:"port"`
		}
		if err := json.Unmarshal([]byte(p.LogLine), &finalState); err == nil {
			dep := &types.Deployment{
				ID:          p.TaskId,
				TaskID:      p.TaskId,
				NodeAlias:   session.NodeAlias,
				ProjectName: session.ProjectName,
				Domain:      finalState.Domain,
				Port:        finalState.Port,
				Status:      "RUNNING",
				CreatedAt:   time.Now(),
			}
			b.dbManager.SaveDeployment(dep)
		}
		return
	case "SUCCESS":
		text = "🎉 **部署任务成功！**\n" + p.LogLine
		go b.handleDeploySuccess(p.TaskId, session)
		return
	case "FAILED":
		text = "❌ **部署任务失败！**\n" + p.LogLine
		go b.handleDeployFailure(p.TaskId, session, p.LogLine)
		return
	default:
		text = fmt.Sprintf("[%s] %s", p.State, p.LogLine)
	}

	b.editMessageThrottled(session.ChatID, session.MessageID, text)
}

func (b *Bot) handleDeploySuccess(taskID string, session *DeploymentSession) {
	b.editMessageDirect(session.ChatID, session.MessageID, "🎉 **部署成功！正在配置域名与 SSL...**")

	if session.CFDNSType != "none" {
		ownerUser, errUser := b.dbManager.GetUser(session.OwnerUID)
		if errUser != nil || ownerUser == nil || ownerUser.AssignedCF == "" {
			b.editMessageDirect(session.ChatID, session.MessageID, "⚠️ **DNS 自动绑定未执行**\n未找到该用户分配的 Cloudflare 配置，请联系管理员分配。")
			time.Sleep(3 * time.Second)
		} else {
			cfConfig, err := b.dbManager.GetCFConfig(ownerUser.AssignedCF)
			if err == nil && cfConfig != nil && cfConfig.APIToken != "" && cfConfig.ZoneID != "" {
				node, nodeErr := b.dbManager.GetNode(session.NodeAlias)
				if nodeErr == nil && node.IP != "" {
					proxied := (session.CFDNSType == "proxy")
					dnsClient := cloudflare.NewDNSClient(cfConfig.APIToken, cfConfig.ZoneID)
					dnsErr := dnsClient.CreateOrUpdateRecord(session.Domain, node.IP, proxied)
					if dnsErr != nil {
						log.Printf("[Cloudflare] DNS resolution failed for %s -> %s: %v", session.Domain, node.IP, dnsErr)
						b.editMessageDirect(session.ChatID, session.MessageID, fmt.Sprintf("⚠️ **DNS 自动绑定失败**\n项目部署成功，但 Cloudflare 解析失败：%v", dnsErr))
						time.Sleep(3 * time.Second)
					} else {
						log.Printf("[Cloudflare] DNS resolution success for %s -> %s using config %s", session.Domain, node.IP, cfConfig.Name)
					}
				}
			} else {
				b.editMessageDirect(session.ChatID, session.MessageID, "⚠️ **DNS 自动绑定失败**\nCloudflare 配置无效或读取失败。")
				time.Sleep(3 * time.Second)
			}
		}
	}

	b.editMessageDirect(session.ChatID, session.MessageID, "🎉 **部署成功！正在生成上线卡片...**")

	time.Sleep(5 * time.Second)

	b.deleteMessage(session.ChatID, session.MessageID)
	if session.UserMsgID > 0 {
		b.deleteMessage(session.ChatID, session.UserMsgID)
	}

	protocolStr := "http"
	if session.UseSSL {
		protocolStr = "https"
	}

	finalCard := fmt.Sprintf(`🚀 **项目部署上线成功！**

📝 **项目名称**: %s
🖥️ **部署节点**: %s
🌐 **访问地址**: %s://%s
⏰ **完成时间**: %s`,
		session.ProjectName,
		session.NodeAlias,
		protocolStr,
		session.Domain,
		time.Now().Format("2006-01-02 15:04:05"),
	)

	user, _ := b.dbManager.GetUser(session.OwnerUID)
	b.replyWithMarkup(session.ChatID, finalCard, b.getMainMenuMarkup(user))

	b.sessionsMu.Lock()
	delete(b.sessions, taskID)
	b.sessionsMu.Unlock()

	b.triggerNextJob(session.NodeAlias, taskID)
}

func (b *Bot) handleDeployFailure(taskID string, session *DeploymentSession, lastError string) {
	b.editMessageDirect(session.ChatID, session.MessageID, "❌ **部署失败！正在打包日志文件...**")

	timestamp := time.Now().Format("2006-01-02_15-04")
	fileName := fmt.Sprintf("error_log_%s.txt", timestamp)
	tempFile := filepath.Join(os.TempDir(), fileName)

	logData := fmt.Sprintf("GOPASS AGENT BUILD FAILURE LOG\n")
	logData += fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339))
	logData += fmt.Sprintf("Project: %s\n", session.ProjectName)
	logData += fmt.Sprintf("Node: %s\n", session.NodeAlias)
	logData += fmt.Sprintf("Reason: %s\n\n", lastError)
	logData += "--- LOG DETAILS ---\n"
	for _, l := range session.LogLines {
		logData += l + "\n"
	}

	_ = os.WriteFile(tempFile, []byte(logData), 0644)
	defer os.Remove(tempFile)

	user, _ := b.dbManager.GetUser(session.OwnerUID)
	b.sendDocumentWithMarkup(session.ChatID, tempFile, fmt.Sprintf("⚠️ 部署出错。错误原因: %s", lastError), b.getMainMenuMarkup(user))

	time.Sleep(3 * time.Second)
	b.deleteMessage(session.ChatID, session.MessageID)
	if session.UserMsgID > 0 {
		b.deleteMessage(session.ChatID, session.UserMsgID)
	}

	b.sessionsMu.Lock()
	delete(b.sessions, taskID)
	b.sessionsMu.Unlock()

	b.triggerNextJob(session.NodeAlias, taskID)
}

func (b *Bot) editMessageThrottled(chatID int64, messageID int, text string) {
	b.mu.Lock()
	lastTime := b.lastEdit[messageID]
	now := time.Now()
	diff := now.Sub(lastTime)
	b.mu.Unlock()

	if diff < 1200*time.Millisecond {
		time.Sleep(1200*time.Millisecond - diff)
	}

	b.mu.Lock()
	b.lastEdit[messageID] = time.Now()
	b.mu.Unlock()

	b.editMessageDirect(chatID, messageID, text)
}

func (b *Bot) editMessageDirect(chatID int64, messageID int, text string) {
	if b.useMock {
		log.Printf("[Bot Mock] Msg [ID: %d] EDITED: %s", messageID, text)
		return
	}

	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.ParseMode = "Markdown"
	_, _ = b.api.Send(msg)
}

func (b *Bot) deleteMessage(chatID int64, messageID int) {
	if b.useMock {
		log.Printf("[Bot Mock] Msg [ID: %d] DELETED", messageID)
		return
	}

	delMsg := tgbotapi.NewDeleteMessage(chatID, messageID)
	_, _ = b.api.Send(delMsg)
}

func (b *Bot) reply(chatID int64, text string) {
	if b.useMock {
		log.Printf("[Bot Mock] REPLY to Chat %d: %s", chatID, text)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	_, _ = b.api.Send(msg)
}

func (b *Bot) replyWithMarkup(chatID int64, text string, replyMarkup interface{}) {
	if b.useMock {
		log.Printf("[Bot Mock] REPLY to Chat %d (with markup): %s", chatID, text)
		return
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if replyMarkup != nil {
		msg.ReplyMarkup = replyMarkup
	}
	_, _ = b.api.Send(msg)
}

func (b *Bot) sendProgressMsg(chatID int64, text string) int {
	if b.useMock {
		msgID := int(time.Now().UnixNano() % 10000)
		log.Printf("[Bot Mock] Msg [ID: %d] CREATED: %s", msgID, text)
		return msgID
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	sent, err := b.api.Send(msg)
	if err != nil {
		return 0
	}
	return sent.MessageID
}

func (b *Bot) sendProgressMsgWithMenu(chatID int64, text string, user *types.User) int {
	if b.useMock {
		msgID := int(time.Now().UnixNano() % 10000)
		log.Printf("[Bot Mock] Msg [ID: %d] CREATED with Menu: %s", msgID, text)
		return msgID
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if user != nil {
		msg.ReplyMarkup = b.getMainMenuMarkup(user)
	}
	sent, err := b.api.Send(msg)
	if err != nil {
		return 0
	}
	return sent.MessageID
}

func (b *Bot) sendDocument(chatID int64, filePath, caption string) {
	if b.useMock {
		log.Printf("[Bot Mock] DOCUMENT uploaded to Chat %d: %s (Caption: %s)", chatID, filepath.Base(filePath), caption)
		return
	}

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(filePath))
	doc.Caption = caption
	_, _ = b.api.Send(doc)
}

func (b *Bot) sendDocumentWithMarkup(chatID int64, filePath, caption string, replyMarkup interface{}) {
	if b.useMock {
		log.Printf("[Bot Mock] DOCUMENT uploaded (with markup) to Chat %d: %s (Caption: %s)", chatID, filepath.Base(filePath), caption)
		return
	}

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(filePath))
	doc.Caption = caption
	if replyMarkup != nil {
		doc.ReplyMarkup = replyMarkup
	}
	_, _ = b.api.Send(doc)
}

func parseGitProject(gitURL string) string {
	parts := strings.Split(gitURL, "/")
	if len(parts) == 0 {
		return "app"
	}
	last := parts[len(parts)-1]
	last = strings.TrimSuffix(last, ".git")
	last = strings.ToLower(last)
	reg := regexp.MustCompile(`[^a-z0-9-]`)
	last = reg.ReplaceAllString(last, "-")
	if last == "" {
		return "app"
	}
	return last
}

func (b *Bot) onNodeHeartbeat(alias string, cpu, mem float64) {
	if !IsResourceRecovered(cpu, mem) {
		return
	}

	q := b.qc.GetQueue(alias)
	active := q.GetActive()
	if active != nil && !active.Dispatched {
		active.Dispatched = true
		log.Printf("[Queue] Node '%s' load recovered (CPU: %.2f%%, Mem: %.2f%%). Triggering pending active job: %s",
			alias, cpu, mem, active.TaskID)
		b.editMessageDirect(active.ChatID, active.MessageID, "📥 **节点资源已回落安全线，任务自动激活构建中...**")
		go func(j *DeployJob) {
			if err := j.ExecuteFunc(j); err != nil {
				b.onTaskProgress(&pb.TaskProgress{
					TaskId:  j.TaskID,
					State:   "FAILED",
					LogLine: "gRPC dispatch failed: " + err.Error(),
				})
			}
		}(active)
	}
}

func (b *Bot) triggerNextJob(nodeAlias string, completedTaskID string) {
	_, nextJob, hasNext := b.qc.GetQueue(nodeAlias).Complete(completedTaskID)
	if !hasNext {
		return
	}

	node, err := b.dbManager.GetNode(nodeAlias)
	var nodeCPU, nodeMem float64
	if err == nil {
		nodeCPU = node.Hardware.CPUUsage
		nodeMem = node.Hardware.MemoryUsage
	}

	errOverload := CheckResourceOverload(nodeCPU, nodeMem)
	if errOverload != nil {
		b.editMessageDirect(nextJob.ChatID, nextJob.MessageID, fmt.Sprintf("⚠️ **目标节点负载高，排队任务挂起中**\n排队任务已移至队首，等待节点降温后自动唤醒... (CPU: %.1f%%, 内存: %.1f%%)", nodeCPU, nodeMem))
		return
	}

	nextJob.Dispatched = true
	b.editMessageDirect(nextJob.ChatID, nextJob.MessageID, "📥 **您的任务已从排队中唤醒，开始构建...**")
	go func(j *DeployJob) {
		if err := j.ExecuteFunc(j); err != nil {
			b.onTaskProgress(&pb.TaskProgress{
				TaskId:  j.TaskID,
				State:   "FAILED",
				LogLine: "gRPC dispatch failed: " + err.Error(),
			})
		}
	}(nextJob)
}

// extractUIDFromButton parses UID from button text that may be in format "Name (UID)" or plain "UID"
func extractUIDFromButton(text string) (int64, error) {
	text = strings.TrimSpace(text)
	// Try plain numeric first
	if uid, err := strconv.ParseInt(text, 10, 64); err == nil {
		return uid, nil
	}
	// Try "Name (UID)" format
	if idx := strings.LastIndex(text, "("); idx >= 0 {
		inner := strings.TrimSuffix(strings.TrimSpace(text[idx+1:]), ")")
		inner = strings.TrimSpace(inner)
		return strconv.ParseInt(inner, 10, 64)
	}
	return 0, fmt.Errorf("cannot extract UID from: %s", text)
}

func (b *Bot) startSSHDeployProcess(chatID int64, user *types.User, state *UserConversationState, masterAddr string) {
	statusMsg, err := b.api.Send(tgbotapi.NewMessage(chatID, "⏳ **[1/3] 正在本地交叉编译 Linux 被控端...**"))
	var statusMsgID int
	if err == nil {
		statusMsgID = statusMsg.MessageID
	}

	updateStatus := func(text string) {
		if statusMsgID > 0 {
			msg := tgbotapi.NewEditMessageText(chatID, statusMsgID, text)
			msg.ParseMode = "Markdown"
			_, _ = b.api.Send(msg)
		} else {
			log.Printf("[Bot Status] %s", text)
		}
	}

	// Step 1: Compile agent for Linux
	agentBin, err := CompileAgentForLinux()
	if err != nil {
		updateStatus(fmt.Sprintf("❌ **编译失败**\n\n项目在本地交叉编译 Linux 程序时出错:\n`%v`", err))
		return
	}

	// Step 2: SSH Upload and Deploy
	updateStatus("⏳ **[2/3] 编译成功。正在通过 SSH 安全信道上传并启动被控端...**")

	isPrivateKey := strings.Contains(state.SSHAuth, "-----BEGIN")
	var password, privateKey string
	if isPrivateKey {
		privateKey = state.SSHAuth
	} else {
		password = state.SSHAuth
	}

	err = DeployAgentToRemote(
		state.SSHHost,
		state.SSHPort,
		state.SSHUser,
		password,
		privateKey,
		agentBin,
		masterAddr,
		b.token,
		state.SSHNodeAlias,
		b.gRPCServer.tlsEnabled,
		b.gRPCServer.tlsCertPath == "" && b.gRPCServer.tlsKeyPath == "",
	)
	if err != nil {
		updateStatus(fmt.Sprintf("❌ **部署失败**\n\nSSH 安装被控端出错:\n`%v`", err))
		return
	}

	// Step 3: Wait for registration handshake
	updateStatus("⏳ **[3/3] 安装成功。正在等待 gRPC 握手与安全 TLS 通信建立...**")

	success := false
	for i := 0; i < 15; i++ {
		time.Sleep(1 * time.Second)
		node, err := b.dbManager.GetNode(state.SSHNodeAlias)
		if err == nil && node.Connected {
			success = true
			break
		}
	}

	if success {
		updateStatus(fmt.Sprintf("🎉 **新节点安全联通！**\n\n被控端节点 `[ %s ]` 已成功通过 TLS 安全加密信道接入 Master 并正常开始心跳上报！", state.SSHNodeAlias))
	} else {
		updateStatus(fmt.Sprintf("❌ **联通超时**\n\n被控端程序已在远程主机启动，但未能在 15 秒内回连至 Master。\n\n请检查：\n1. 目标服务器与 Master (%s) 之间的网络连通性及防火墙端口规则。\n2. 远程主机上的日志文件 `~/gopass-agent.log`。", masterAddr))
	}
}

// detectMasterIP attempts to retrieve the public IP of the Master, falling back to local IPs if offline.
func detectMasterIP() string {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("https://api.ipify.org")
	if err == nil {
		defer resp.Body.Close()
		ip, err := io.ReadAll(resp.Body)
		if err == nil && len(ip) > 0 {
			return string(ip)
		}
	}

	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, address := range addrs {
			if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
	}

	return "YOUR_MASTER_PUBLIC_IP"
}

func (b *Bot) handleFetchCFZones(chatID int64, fromUID int64, user *types.User) {
	if user.AssignedCF == "" {
		b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CUSTOM_DOMAIN", "✏️ 您未绑定 Cloudflare 配置，请输入完整自定义独立域名 (例如 app.example.com):", b.getCancelKeyboard())
		return
	}

	cfConfig, err := b.dbManager.GetCFConfig(user.AssignedCF)
	if err != nil || cfConfig == nil {
		b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CUSTOM_DOMAIN", "✏️ 读取 Cloudflare 配置失败，请输入完整自定义独立域名:", b.getCancelKeyboard())
		return
	}

	b.editMessageDirect(chatID, -1, "☁️ 正在抓取 Cloudflare 域名列表，请稍候...")
	zones, err := cloudflare.ListZones(cfConfig.APIToken)
	if err != nil || len(zones) == 0 {
		b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CUSTOM_DOMAIN", "✏️ 抓取域名失败或无可用域名，请输入完整自定义独立域名:", b.getCancelKeyboard())
		return
	}

	var replyButtons [][]tgbotapi.KeyboardButton
	for _, z := range zones {
		replyButtons = append(replyButtons, []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("🌐 " + z.Name)})
	}
	replyButtons = append(replyButtons, []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("❌ 取消操作")})

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	b.userStatesMu.Lock()
	state, exists := b.userStates[fromUID]
	if exists {
		state.Step = "WAITING_FOR_CF_ZONE_SELECTION"
	}
	b.userStatesMu.Unlock()

	b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_ZONE_SELECTION", "☁️ 请选择要绑定的主域名:", replyMarkup)
}
