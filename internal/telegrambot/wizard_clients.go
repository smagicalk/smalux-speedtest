package telegrambot

import (
	"context"
	"fmt"
	"sort"
	"strconv"
)

const clientPageSize = 6

func (b *Bot) beginClientSelection(ctx context.Context, userID int64, state *wizardState) {
	provider, ok := b.runner.(ClientProvider)
	if !ok {
		b.failWizard(ctx, userID, state, "当前服务不支持选择测速节点。")
		return
	}
	clients, err := provider.ListAvailableClients(ctx)
	if err != nil {
		b.failWizard(ctx, userID, state, "读取在线测速节点失败，请稍后重试。")
		return
	}
	if len(clients) == 0 {
		b.failWizard(ctx, userID, state, "当前没有在线且已启用的测速节点。")
		return
	}
	sort.SliceStable(clients, func(i, j int) bool {
		if clients[i].Name == clients[j].Name {
			return clients[i].ID < clients[j].ID
		}
		return clients[i].Name < clients[j].Name
	})
	state.clients = clients
	state.selectedClients = make(map[string]bool, len(clients))
	for _, client := range clients {
		state.selectedClients[client.ID] = true
	}
	state.clientPage = 0
	state.stage = wizardClients

	if state.controlMessageID != 0 {
		deleteCtx, cancelDelete := context.WithTimeout(context.WithoutCancel(ctx), textDeliveryTimeout)
		_ = b.deleteMessageOrdered(deleteCtx, state.chatID, state.controlMessageID)
		cancelDelete()
	}
	deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), textDeliveryTimeout)
	sent, sendErr := b.sendMessageOrdered(deliveryCtx, MessageRequest{
		ChatID: state.chatID, Text: clientSelectionText(state),
		ReplyParameters: reply(state.requestMessageID), ReplyMarkup: clientKeyboard(state),
	})
	cancel()
	if sendErr != nil || sent.MessageID == 0 {
		b.clearWizard(userID)
		return
	}
	state.controlMessageID = sent.MessageID
	b.storeWizard(userID, state)
}

func (b *Bot) handleClientCallback(ctx context.Context, query *CallbackQuery, userID int64, state *wizardState, action string, parts []string) bool {
	switch action {
	case "client":
		if len(parts) != 4 {
			return true
		}
		index, err := strconv.Atoi(parts[3])
		if err != nil || index < 0 || index >= len(state.clients) {
			return true
		}
		id := state.clients[index].ID
		state.selectedClients[id] = !state.selectedClients[id]
	case "client-page":
		if len(parts) != 4 {
			return true
		}
		page, err := strconv.Atoi(parts[3])
		if err != nil || page < 0 || page >= clientPageCount(state) {
			return true
		}
		state.clientPage = page
	case "client-all":
		for _, client := range state.clients {
			state.selectedClients[client.ID] = true
		}
	case "client-none":
		clear(state.selectedClients)
	case "client-next":
		if len(selectedClientIDs(state)) == 0 {
			_ = b.api.AnswerCallbackQuery(ctx, query.ID, "请至少选择一个测速节点。")
			return true
		}
		state.stage = wizardCandidates
		b.storeWizard(userID, state)
		b.editWizardMessage(ctx, query.Message, "选择用于延迟筛选的候选测速节点数：", candidateKeyboard(state.id))
		return true
	default:
		return false
	}
	b.storeWizard(userID, state)
	b.editWizardMessage(ctx, query.Message, clientSelectionText(state), clientKeyboard(state))
	return true
}

func (b *Bot) failWizard(ctx context.Context, userID int64, state *wizardState, text string) {
	messageID := state.controlMessageID
	b.clearWizard(userID)
	if messageID != 0 {
		_ = b.editMessageOrdered(ctx, EditMessageRequest{ChatID: state.chatID, MessageID: messageID, Text: text, ReplyMarkup: mainMenu()})
		return
	}
	b.sendMainMenu(ctx, state.chatID, text)
}

func clientSelectionText(state *wizardState) string {
	pages := clientPageCount(state)
	return fmt.Sprintf("选择测速节点（可多选）\n已选择 %d / %d 个 Client · 第 %d / %d 页", len(selectedClientIDs(state)), len(state.clients), state.clientPage+1, pages)
}

func clientKeyboard(state *wizardState) *InlineKeyboardMarkup {
	start := state.clientPage * clientPageSize
	end := min(start+clientPageSize, len(state.clients))
	rows := make([][]InlineKeyboardButton, 0, clientPageSize+4)
	for index := start; index < end; index++ {
		client := state.clients[index]
		mark := "○"
		if state.selectedClients[client.ID] {
			mark = "✓"
		}
		name := sanitizeTelegramLabel(client.Name, 32)
		if name == "" {
			name = "未命名节点"
		}
		rows = append(rows, buttonRow(button(mark+" "+name, wizardData(state.id, "client", strconv.Itoa(index)))))
	}
	pages := clientPageCount(state)
	if pages > 1 {
		navigation := make([]InlineKeyboardButton, 0, 2)
		if state.clientPage > 0 {
			navigation = append(navigation, button("上一页", wizardData(state.id, "client-page", strconv.Itoa(state.clientPage-1))))
		}
		if state.clientPage+1 < pages {
			navigation = append(navigation, button("下一页", wizardData(state.id, "client-page", strconv.Itoa(state.clientPage+1))))
		}
		rows = append(rows, buttonRow(navigation...))
	}
	rows = append(rows,
		buttonRow(button("全选", wizardData(state.id, "client-all")), button("清空", wizardData(state.id, "client-none"))),
		buttonRow(button("继续", wizardData(state.id, "client-next")), button("取消", wizardData(state.id, "cancel"))),
	)
	return keyboard(rows...)
}

func clientPageCount(state *wizardState) int {
	return max(1, (len(state.clients)+clientPageSize-1)/clientPageSize)
}

func selectedClientIDs(state *wizardState) []string {
	ids := make([]string, 0, len(state.clients))
	for _, client := range state.clients {
		if state.selectedClients[client.ID] {
			ids = append(ids, client.ID)
		}
	}
	return ids
}
