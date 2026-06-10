package master

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
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
	DeployCFDNSType string // "proxy", "dns", "none"
	DeployUseSSL    bool
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
func NewBot(dbManager *db.Manager, gRPCServer *Server, token string, useMock bool) (*Bot, error) {
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
		dbManager:  dbManager,
		gRPCServer: gRPCServer,
		token:      token,
		api:        api,
		useMock:    useMock || (api == nil),
		lastEdit:   make(map[int]time.Time),
		sessions:   make(map[string]*DeploymentSession),
		qc:         NewQueueCoordinator(),
		stopChan:   make(chan struct{}),
		userStates: make(map[int64]*UserConversationState),
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

			if text == "🌐 默认子域名" {
				b.userStatesMu.Lock()
				state.DeployDomain = "" // 使用默认子域名
				state.UpdatedAt = time.Now()
				b.userStatesMu.Unlock()

				// 下一步：Cloudflare 解析配置
				b.showCFDNSTypeMenu(chatID, fromUID)
			} else if text == "✏️ 自定义独立域名" {
				b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CUSTOM_DOMAIN", "✏️ 请输入自定义域名:", b.getCancelKeyboard())
			} else {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ 输入无效，请重新选择:")
			}
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

			token := strings.TrimSpace(text)
			if token == "" || strings.Contains(token, " ") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ Token 格式错误，请重新输入:")
				return
			}

			b.userStatesMu.Lock()
			state.DeployGitURL = token // 借用 GitURL 存 Token
			b.userStatesMu.Unlock()

			b.updateWizardPrompt(chatID, fromUID, "WAITING_FOR_CF_ZONE", "☁️ 请输入 Cloudflare Zone ID:", b.getCancelKeyboard())
			return
		}

		if state.Step == "WAITING_FOR_CF_ZONE" {
			b.userStatesMu.Lock()
			state.UserMsgIDs = append(state.UserMsgIDs, msgID)
			b.userStatesMu.Unlock()

			zone := strings.TrimSpace(text)
			if zone == "" || strings.Contains(zone, " ") {
				b.sendErrorReplyWithCancel(chatID, fromUID, "❌ Zone ID 格式错误，请重新输入:")
				return
			}

			token := state.DeployGitURL
			err := b.dbManager.SetCFConfig(token, zone)
			
			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			b.clearConversationHistory(chatID, state)

			if err != nil {
				b.reply(chatID, "❌ Cloudflare 配置保存失败: "+err.Error())
			} else {
				if b.useMock {
					log.Printf("[Bot Mock] REPLY: Cloudflare 配置保存成功！")
				} else {
					msg := tgbotapi.NewMessage(chatID, "🎉 **Cloudflare API 凭证配置保存成功！**\n系统将在后续部署中自动调用 API 绑定解析记录。")
					msg.ReplyMarkup = b.getMainMenuMarkup(user)
					_, _ = b.api.Send(msg)
				}
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
			errSave := b.dbManager.SaveUser(targetUser)

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			b.clearConversationHistory(chatID, state)

			if errSave != nil {
				b.reply(chatID, "❌ 权限升级失败: "+errSave.Error())
			} else {
				if b.useMock {
					log.Printf("[Bot Mock] REPLY: 用户已升级为管理员。")
				} else {
					msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("👑 **已成功将子账户 (UID: `%d`) 升级为副管理员！**\n该账户现在已具备部署应用和生成邀请码等管理权限。", uid))
					msg.ReplyMarkup = b.getMainMenuMarkup(user)
					_, _ = b.api.Send(msg)
				}
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

			errSave := b.dbManager.SaveUser(targetUser)

			b.userStatesMu.Lock()
			delete(b.userStates, fromUID)
			b.userStatesMu.Unlock()

			b.clearConversationHistory(chatID, state)

			if errSave != nil {
				b.reply(chatID, "❌ 直接增添管理员失败: "+errSave.Error())
			} else {
				if b.useMock {
					log.Printf("[Bot Mock] REPLY: 直接增添管理员成功。")
				} else {
					msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("👑 **已成功直接将用户 (UID: `%d`) 增添为副管理员！**\n该账户现在已具备部署应用和生成邀请码等管理权限。", uid))
					msg.ReplyMarkup = b.getMainMenuMarkup(user)
					_, _ = b.api.Send(msg)
				}
			}
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
		if user.Role != "master" && user.Role != "admin" {
			b.reply(chatID, "❌ 权限不足。仅管理员可生成邀请码。")
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

		addr := b.gRPCServer.addr
		displayAddr := addr
		if strings.HasPrefix(addr, "127.0.0.1") || strings.HasPrefix(addr, "0.0.0.0") || strings.HasPrefix(addr, ":") {
			displayAddr = "YOUR_MASTER_PUBLIC_IP" + strings.TrimPrefix(addr, "127.0.0.1")
			displayAddr = strings.TrimPrefix(displayAddr, "0.0.0.0")
			displayAddr = strings.TrimPrefix(displayAddr, ":")
			if !strings.Contains(displayAddr, ":") {
				displayAddr = "YOUR_MASTER_PUBLIC_IP:50051"
			}
		}

		var sb strings.Builder
		sb.WriteString("➕ **添加新服务器 (零配置快速集成)**\n")
		sb.WriteString("————————————————————\n")
		sb.WriteString("请将以下命令复制到新节点的终端中运行，即可拉起被控端并注册至当前集群：\n\n")
		sb.WriteString("👉 **Windows 终端一键启动**:\n")
		sb.WriteString(fmt.Sprintf("`.\\gopass-agent.exe -master %s -token %s -alias 新服务器别名`\n\n", displayAddr, b.token))
		sb.WriteString("👉 **Linux 终端一键启动**:\n")
		sb.WriteString(fmt.Sprintf("`./gopass-agent -master %s -token %s -alias 新服务器别名`\n\n", displayAddr, b.token))
		sb.WriteString("💡 *提示*：请将命令中的 `YOUR_MASTER_PUBLIC_IP` 替换为您 Master 控制端服务的实际公网公有 IP 地址。")

		msgCfg := tgbotapi.NewMessage(chatID, sb.String())
		msgCfg.ParseMode = "Markdown"
		msgCfg.ReplyMarkup = b.getMainMenuMarkup(user)
		_, _ = b.api.Send(msgCfg)

	case text == "👥 账户管理":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。仅最高管理员主账户可管理账户。")
			return
		}
		b.showAccountMenu(chatID, user)

	case text == "📜 查看子账户":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。仅最高管理员可查看账户。")
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
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。")
			return
		}
		b.showUserSelectionKeyboard(chatID, fromUID, "WAITING_FOR_ADMIN_UID", "👑 **请在下方选择或手动输入要提升为副管理员的子账户 UID：**")

	case text == "➕ 增添管理员":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
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
		if user.Role != "master" {
			b.reply(chatID, "❌ 权限不足。仅最高管理员可访问配置板块。")
			return
		}
		b.showMoreFunctionsMenu(chatID, user)

	case text == "☁️ 配置 Cloudflare":
		b.deleteMessage(chatID, msgID)
		if user.Role != "master" {
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
	if user != nil && user.Role == "master" {
		row1 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("🚀 部署应用"), tgbotapi.NewKeyboardButton("🔑 生成邀请码")}
		row2 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("🖥️ 节点状态"), tgbotapi.NewKeyboardButton("👥 账户管理")}
		row3 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("⚙️ 更多功能"), tgbotapi.NewKeyboardButton("❌ 取消操作")}
		replyButtons = append(replyButtons, row1, row2, row3)
	} else if user != nil && user.Role == "admin" {
		row1 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("🚀 部署应用"), tgbotapi.NewKeyboardButton("🔑 生成邀请码")}
		row2 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("🖥️ 节点状态"), tgbotapi.NewKeyboardButton("❌ 取消操作")}
		replyButtons = append(replyButtons, row1, row2)
	} else {
		row1 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("🚀 部署应用"), tgbotapi.NewKeyboardButton("🖥️ 节点状态")}
		row2 := []tgbotapi.KeyboardButton{tgbotapi.NewKeyboardButton("❌ 取消操作")}
		replyButtons = append(replyButtons, row1, row2)
	}

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
		tgbotapi.NewKeyboardButton("🌐 默认子域名"),
		tgbotapi.NewKeyboardButton("✏️ 自定义独立域名"),
	}
	row2 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("❌ 取消操作"),
	}
	replyButtons = append(replyButtons, row1, row2)

	replyMarkup := tgbotapi.NewReplyKeyboard(replyButtons...)
	replyMarkup.ResizeKeyboard = true

	text := "🌐 请选择域名解析方式:"
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
	row1 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("☁️ 配置 Cloudflare"),
		tgbotapi.NewKeyboardButton("📜 查看 Master 日志"),
	}
	row2 := []tgbotapi.KeyboardButton{
		tgbotapi.NewKeyboardButton("⬅️ 返回主菜单"),
	}
	replyButtons = append(replyButtons, row1, row2)

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
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	activationCode := strings.ToUpper(hex.EncodeToString(buf))

	tok := &types.Token{
		Hash:      activationCode,
		MaxUses:   maxUses,
		UsedCount: 0,
		CreatedBy: creatorUID,
		CreatedAt: time.Now(),
	}

	if err := b.dbManager.SaveToken(tok); err != nil {
		b.reply(chatID, "❌ 数据库保存激活码失败: "+err.Error())
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
		cfToken, cfZone, err := b.dbManager.GetCFConfig()
		if err == nil && cfToken != "" && cfZone != "" {
			node, nodeErr := b.dbManager.GetNode(session.NodeAlias)
			if nodeErr == nil && node.IP != "" {
				proxied := (session.CFDNSType == "proxy")
				dnsClient := cloudflare.NewDNSClient(cfToken, cfZone)
				dnsErr := dnsClient.CreateOrUpdateRecord(session.Domain, node.IP, proxied)
				if dnsErr != nil {
					log.Printf("[Cloudflare] DNS resolution failed for %s -> %s: %v", session.Domain, node.IP, dnsErr)
					b.editMessageDirect(session.ChatID, session.MessageID, fmt.Sprintf("⚠️ **DNS 自动绑定失败**\n项目部署成功，但 Cloudflare 解析失败：%v", dnsErr))
					time.Sleep(3 * time.Second)
				} else {
					log.Printf("[Cloudflare] DNS resolution success for %s -> %s", session.Domain, node.IP)
				}
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
