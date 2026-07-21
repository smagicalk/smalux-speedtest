package telegrambot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const (
	wizardAwaitSource = "source"
	wizardAwaitSub    = "subscription"
	wizardClients     = "clients"
	wizardCandidates  = "candidates"
	wizardTopN        = "top_n"
	wizardThreads     = "threads"
	wizardConfirm     = "confirm"
)

type wizardState struct {
	id               string
	chatID           int64
	requestMessageID int64
	controlMessageID int64
	stage            string
	source           []byte
	subscriptionURL  string
	candidateCount   int
	topN             int
	threads          int
	clients          []ClientOption
	selectedClients  map[string]bool
	clientPage       int
}

func botCommands() []BotCommand {
	return []BotCommand{
		{Command: "start", Description: "打开测速菜单"},
		{Command: "test", Description: "提交代理或批量内容"},
		{Command: "sub", Description: "提交订阅 URL"},
		{Command: "cancel", Description: "取消当前测速或配置"},
		{Command: "id", Description: "查看 Telegram ID"},
		{Command: "help", Description: "查看使用说明"},
		{Command: "users", Description: "Owner 查看授权用户"},
		{Command: "authorize", Description: "Owner 授权用户"},
		{Command: "revoke", Description: "Owner 撤销用户"},
	}
}

func mainMenu() *InlineKeyboardMarkup {
	return keyboard(
		buttonRow(button("代理测速", "menu:test"), button("订阅测速", "menu:sub")),
		buttonRow(button("使用帮助", "menu:help")),
	)
}

func (b *Bot) sendMainMenu(ctx context.Context, chatID int64, text string) {
	b.sendTextRequest(ctx, MessageRequest{ChatID: chatID, Text: text, ReplyMarkup: mainMenu()})
}

func (b *Bot) handleWizardInput(ctx context.Context, message *Message, principal Principal, command, payload string, isCommand bool) bool {
	if command == "test" || command == "speedtest" {
		if strings.TrimSpace(payload) != "" {
			return false
		}
		if !b.ensureWizardAuthorized(ctx, message.Chat.ID, principal.UserID) {
			return true
		}
		state := b.newWizard(message.Chat.ID)
		state.stage = wizardAwaitSource
		b.sendWizardPrompt(ctx, principal.UserID, state, "请发送代理分享链接、批量文本或 Base64 内容。")
		return true
	}
	if command == "sub" || command == "subscription" {
		if strings.TrimSpace(payload) != "" {
			return false
		}
		if !b.ensureWizardAuthorized(ctx, message.Chat.ID, principal.UserID) {
			return true
		}
		state := b.newWizard(message.Chat.ID)
		state.stage = wizardAwaitSub
		b.sendWizardPrompt(ctx, principal.UserID, state, "请发送 HTTP(S) 订阅 URL。")
		return true
	}
	if isCommand {
		return false
	}
	if !b.ensureWizardAuthorized(ctx, message.Chat.ID, principal.UserID) {
		return true
	}
	state := b.loadWizard(principal.UserID)
	if state == nil {
		return false
	} else {
		switch state.stage {
		case wizardAwaitSource:
			state.source = []byte(strings.TrimSpace(message.Text))
		case wizardAwaitSub:
			if !isHTTPURL(message.Text) {
				b.sendText(ctx, message.Chat.ID, "订阅地址必须是有效的 HTTP(S) URL，请重新发送。")
				return true
			}
			state.subscriptionURL = strings.TrimSpace(message.Text)
		default:
			return false
		}
	}
	state.requestMessageID = message.MessageID
	b.beginClientSelection(ctx, principal.UserID, state)
	return true
}

func (b *Bot) handleCallback(ctx context.Context, query *CallbackQuery) {
	if query == nil || query.ID == "" || query.Message == nil || query.Message.Chat.Type != "private" {
		return
	}
	_ = b.api.AnswerCallbackQuery(ctx, query.ID, "")
	principal := Principal{UserID: query.From.ID, ChatID: query.Message.Chat.ID, Username: query.From.Username, FirstName: query.From.FirstName, LastName: query.From.LastName}
	switch query.Data {
	case "menu:test":
		b.beginWizardFromButton(ctx, query, principal, wizardAwaitSource)
		return
	case "menu:sub":
		b.beginWizardFromButton(ctx, query, principal, wizardAwaitSub)
		return
	case "menu:help":
		_ = b.editMessageOrdered(ctx, EditMessageRequest{ChatID: principal.ChatID, MessageID: query.Message.MessageID, Text: HelpText, ReplyMarkup: mainMenu()})
		return
	}
	parts := strings.Split(query.Data, ":")
	if len(parts) < 3 || parts[0] != "wiz" {
		return
	}
	state := b.loadWizard(principal.UserID)
	if state == nil || state.id != parts[1] || state.chatID != principal.ChatID {
		_ = b.api.AnswerCallbackQuery(ctx, query.ID, "该配置已过期，请重新开始。")
		return
	}
	action := parts[2]
	if b.handleClientCallback(ctx, query, principal.UserID, state, action, parts) {
		return
	}
	switch action {
	case "cancel":
		b.clearWizard(principal.UserID)
		_ = b.editMessageOrdered(ctx, EditMessageRequest{ChatID: principal.ChatID, MessageID: query.Message.MessageID, Text: "已取消本次测速配置。", ReplyMarkup: mainMenu()})
	case "candidate":
		if len(parts) != 4 {
			return
		}
		state.candidateCount, _ = strconv.Atoi(parts[3])
		if !allowedInt(state.candidateCount, 1, 5, 10, 20, 50) {
			_ = b.api.AnswerCallbackQuery(ctx, query.ID, "无效的候选节点数。")
			return
		}
		state.stage = wizardTopN
		b.storeWizard(principal.UserID, state)
		b.editWizardMessage(ctx, query.Message, "选择实际执行上下行测速的节点数：", topNKeyboard(state.id, state.candidateCount))
	case "top":
		if len(parts) != 4 {
			return
		}
		state.topN, _ = strconv.Atoi(parts[3])
		if state.topN < 1 || state.topN > 3 || state.topN > state.candidateCount {
			_ = b.api.AnswerCallbackQuery(ctx, query.ID, "无效的 Top N。")
			return
		}
		state.stage = wizardThreads
		b.storeWizard(principal.UserID, state)
		b.editWizardMessage(ctx, query.Message, "选择每次测速使用的并发线程数：", threadsKeyboard(state.id))
	case "threads":
		if len(parts) != 4 {
			return
		}
		state.threads, _ = strconv.Atoi(parts[3])
		if !allowedInt(state.threads, 1, 4, 8, 16, 32) {
			_ = b.api.AnswerCallbackQuery(ctx, query.ID, "无效的线程数。")
			return
		}
		state.stage = wizardConfirm
		b.storeWizard(principal.UserID, state)
		b.editWizardMessage(ctx, query.Message, wizardSummary(state), confirmKeyboard(state.id))
	case "back-candidate":
		state.stage = wizardClients
		b.storeWizard(principal.UserID, state)
		b.editWizardMessage(ctx, query.Message, clientSelectionText(state), clientKeyboard(state))
	case "back-top":
		state.stage = wizardTopN
		b.storeWizard(principal.UserID, state)
		b.editWizardMessage(ctx, query.Message, "选择实际执行上下行测速的节点数：", topNKeyboard(state.id, state.candidateCount))
	case "back-threads":
		state.stage = wizardThreads
		b.storeWizard(principal.UserID, state)
		b.editWizardMessage(ctx, query.Message, "选择每次测速使用的并发线程数：", threadsKeyboard(state.id))
	case "confirm":
		request := TaskRequest{Principal: principal, Source: string(state.source), SubscriptionURL: state.subscriptionURL, CandidateCount: state.candidateCount, TopN: state.topN, Threads: state.threads, ClientIDs: selectedClientIDs(state)}
		requestMessageID := state.requestMessageID
		controlMessageID := state.controlMessageID
		b.clearWizard(principal.UserID)
		_ = b.editMessageOrdered(ctx, EditMessageRequest{ChatID: principal.ChatID, MessageID: query.Message.MessageID, Text: "配置已确认，正在创建测速任务…"})
		b.submitRequestWithStatus(ctx, &Message{MessageID: requestMessageID, Chat: Chat{ID: principal.ChatID, Type: "private"}}, principal, request, controlMessageID)
	}
}

func (b *Bot) beginWizardFromButton(ctx context.Context, query *CallbackQuery, principal Principal, stage string) {
	if !b.ensureWizardAuthorized(ctx, principal.ChatID, principal.UserID) {
		return
	}
	state := b.newWizard(principal.ChatID)
	state.stage = stage
	state.controlMessageID = query.Message.MessageID
	b.storeWizard(principal.UserID, state)
	prompt := "请发送代理分享链接、批量文本或 Base64 内容。"
	if stage == wizardAwaitSub {
		prompt = "请发送 HTTP(S) 订阅 URL。"
	}
	b.editWizardMessage(ctx, query.Message, prompt, cancelKeyboard(state.id))
}

func (b *Bot) ensureWizardAuthorized(ctx context.Context, chatID, userID int64) bool {
	if userID == 0 {
		b.sendText(ctx, chatID, "无法识别 Telegram 用户身份。")
		return false
	}
	allowed, err := b.authorization.IsAuthorized(ctx, userID)
	if err != nil {
		b.sendText(ctx, chatID, "权限校验失败，请稍后重试。")
		return false
	}
	if !allowed {
		b.sendText(ctx, chatID, "当前账号未获准提交测速任务。")
		return false
	}
	return true
}

func (b *Bot) editWizardMessage(ctx context.Context, message *Message, text string, markup *InlineKeyboardMarkup) {
	_ = b.editMessageOrdered(ctx, EditMessageRequest{ChatID: message.Chat.ID, MessageID: message.MessageID, Text: text, ReplyMarkup: markup})
}

func candidateKeyboard(id string) *InlineKeyboardMarkup {
	return keyboard(
		buttonRow(button("1", wizardData(id, "candidate", "1")), button("5", wizardData(id, "candidate", "5")), button("10", wizardData(id, "candidate", "10"))),
		buttonRow(button("20", wizardData(id, "candidate", "20")), button("50", wizardData(id, "candidate", "50"))),
		buttonRow(button("取消", wizardData(id, "cancel"))),
	)
}
func topNKeyboard(id string, candidateCount int) *InlineKeyboardMarkup {
	choices := []InlineKeyboardButton{button("Top 1", wizardData(id, "top", "1"))}
	if candidateCount >= 2 {
		choices = append(choices, button("Top 2", wizardData(id, "top", "2")))
	}
	if candidateCount >= 3 {
		choices = append(choices, button("Top 3", wizardData(id, "top", "3")))
	}
	return keyboard(
		buttonRow(choices...),
		buttonRow(button("返回", wizardData(id, "back-candidate")), button("取消", wizardData(id, "cancel"))),
	)
}
func threadsKeyboard(id string) *InlineKeyboardMarkup {
	return keyboard(
		buttonRow(button("1", wizardData(id, "threads", "1")), button("4", wizardData(id, "threads", "4")), button("8", wizardData(id, "threads", "8"))),
		buttonRow(button("16", wizardData(id, "threads", "16")), button("32", wizardData(id, "threads", "32"))),
		buttonRow(button("返回", wizardData(id, "back-top")), button("取消", wizardData(id, "cancel"))),
	)
}
func confirmKeyboard(id string) *InlineKeyboardMarkup {
	return keyboard(
		buttonRow(button("开始测速", wizardData(id, "confirm"))),
		buttonRow(button("返回", wizardData(id, "back-threads")), button("取消", wizardData(id, "cancel"))),
	)
}
func cancelKeyboard(id string) *InlineKeyboardMarkup {
	return keyboard(buttonRow(button("取消", wizardData(id, "cancel"))))
}

func wizardSummary(state *wizardState) string {
	source := "代理 / 批量内容"
	if state.subscriptionURL != "" {
		source = "订阅 URL"
	}
	return fmt.Sprintf("确认测速配置\n来源：%s\n测速节点：%d 个 Client\n候选节点：%d\n上下行节点：Top %d\n线程数：%d", source, len(selectedClientIDs(state)), state.candidateCount, state.topN, state.threads)
}

func keyboard(rows ...[]InlineKeyboardButton) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}
func buttonRow(values ...InlineKeyboardButton) []InlineKeyboardButton { return values }
func button(text, data string) InlineKeyboardButton {
	return InlineKeyboardButton{Text: text, CallbackData: data}
}
func wizardData(id, action string, value ...string) string {
	parts := []string{"wiz", id, action}
	parts = append(parts, value...)
	return strings.Join(parts, ":")
}
func reply(messageID int64) *ReplyParameters {
	if messageID <= 0 {
		return nil
	}
	return &ReplyParameters{MessageID: messageID, AllowSendingWithoutReply: true}
}

func (b *Bot) newWizard(chatID int64) *wizardState {
	return &wizardState{id: wizardID(), chatID: chatID, selectedClients: make(map[string]bool)}
}

func (b *Bot) sendWizardPrompt(ctx context.Context, userID int64, state *wizardState, text string) {
	deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), textDeliveryTimeout)
	sent, err := b.sendMessageOrdered(deliveryCtx, MessageRequest{ChatID: state.chatID, Text: text, ReplyMarkup: cancelKeyboard(state.id)})
	cancel()
	if err == nil {
		state.controlMessageID = sent.MessageID
	}
	b.storeWizard(userID, state)
}
func (b *Bot) loadWizard(userID int64) *wizardState {
	b.wizardMu.Lock()
	defer b.wizardMu.Unlock()
	return b.wizards[userID]
}
func (b *Bot) storeWizard(userID int64, state *wizardState) {
	b.wizardMu.Lock()
	previous := b.wizards[userID]
	b.wizards[userID] = state
	b.wizardMu.Unlock()
	if previous != nil && previous != state {
		wipeWizard(previous)
	}
}
func (b *Bot) clearWizard(userID int64) bool {
	b.wizardMu.Lock()
	state := b.wizards[userID]
	delete(b.wizards, userID)
	b.wizardMu.Unlock()
	wipeWizard(state)
	return state != nil
}
func wipeWizard(state *wizardState) {
	if state != nil {
		for i := range state.source {
			state.source[i] = 0
		}
		state.source = nil
		state.subscriptionURL = ""
	}
}
func allowedInt(value int, allowed ...int) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
func wizardID() string {
	var value [6]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(value[:])
}
