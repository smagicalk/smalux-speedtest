package telegrambot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const progressEditInterval = 2 * time.Second

type clientProgress struct {
	name, phase, message, status string
	current, total               int
	rateBPS                      float64
}

// progressEditor 聚合多个 Client 的独立事件，并用同一条 Telegram 消息展示总体和节点
// 状态。高频传输速率事件最多每两秒编辑一次，避免触发 flood control。
type progressEditor struct {
	bot       *Bot
	chatID    int64
	messageID int64
	task      Task
	mu        sync.Mutex
	lastEdit  time.Time
	lastText  string
	status    string
	results   int
	clients   map[string]*clientProgress
}

func newProgressEditor(bot *Bot, chatID, messageID int64, task Task) *progressEditor {
	editor := &progressEditor{bot: bot, chatID: chatID, messageID: messageID, task: task, status: "queued", clients: make(map[string]*clientProgress)}
	for _, client := range task.Clients {
		editor.clients[client.ID] = &clientProgress{name: client.Name, status: "queued"}
	}
	editor.lastText = editor.render()
	return editor
}

func initialProgressText(task Task) string {
	return newProgressEditor(nil, 0, 0, task).render()
}

func (e *progressEditor) Update(progress TaskProgress) {
	if e == nil || e.messageID == 0 {
		return
	}
	e.mu.Lock()
	if progress.Status != "" {
		e.status = progress.Status
	}
	if progress.Results > e.results {
		e.results = progress.Results
	}
	if progress.ClientID != "" {
		client := e.clients[progress.ClientID]
		if client == nil {
			client = &clientProgress{name: progress.ClientName, status: "queued"}
			e.clients[progress.ClientID] = client
		}
		if progress.ClientName != "" {
			client.name = progress.ClientName
		}
		if progress.Phase != "" {
			client.phase = progress.Phase
		}
		if progress.Message != "" {
			client.message = progress.Message
		}
		if progress.TargetStatus != "" {
			client.status = progress.TargetStatus
		} else if progress.Status == "running" {
			client.status = "running"
		}
		client.current, client.total, client.rateBPS = progress.Current, progress.Total, progress.RateBPS
	}
	text := e.renderLocked()
	if text == e.lastText || (!e.lastEdit.IsZero() && time.Since(e.lastEdit) < progressEditInterval) {
		e.mu.Unlock()
		return
	}
	e.lastEdit, e.lastText = time.Now(), text
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), textDeliveryTimeout)
	_ = e.bot.editMessageOrdered(ctx, EditMessageRequest{ChatID: e.chatID, MessageID: e.messageID, Text: text})
	cancel()
}

func (e *progressEditor) Finish(ctx context.Context, text string) {
	if e == nil || e.messageID == 0 {
		return
	}
	e.mu.Lock()
	if text == e.lastText {
		e.mu.Unlock()
		return
	}
	e.lastText = text
	e.mu.Unlock()
	deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), textDeliveryTimeout)
	_ = e.bot.editMessageOrdered(deliveryCtx, EditMessageRequest{ChatID: e.chatID, MessageID: e.messageID, Text: text})
	cancel()
}

func (e *progressEditor) render() string { e.mu.Lock(); defer e.mu.Unlock(); return e.renderLocked() }

func (e *progressEditor) renderLocked() string {
	totalClients := e.task.ClientCount
	if totalClients == 0 {
		totalClients = len(e.clients)
	}
	finished := 0
	for _, client := range e.clients {
		if terminalTargetStatus(client.status) {
			finished++
		}
	}
	expected := e.task.ProxyCount * totalClients * max(e.task.TopN, 1)
	percent := 0
	if expected > 0 {
		percent = min(100, e.results*100/expected)
	}
	if totalClients > 0 {
		percent = max(percent, finished*100/totalClients)
	}
	filled := min(10, max(0, percent/10))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
	status := telegramProgressStatus(e.status)
	var builder strings.Builder
	fmt.Fprintf(&builder, "测速任务已创建：%s\n状态：%s\n总体：%s %d%% · Client %d/%d", e.task.ID, status, bar, percent, finished, totalClients)
	if expected > 0 {
		fmt.Fprintf(&builder, " · 结果 %d/%d", e.results, expected)
	} else if e.results > 0 {
		fmt.Fprintf(&builder, " · 结果 %d", e.results)
	}
	if len(e.task.Clients) > 0 {
		builder.WriteString("\n\n测速节点：")
		limit := min(8, len(e.task.Clients))
		for _, option := range e.task.Clients[:limit] {
			builder.WriteString("\n")
			builder.WriteString(formatClientProgress(option, e.clients[option.ID]))
		}
		if len(e.task.Clients) > limit {
			fmt.Fprintf(&builder, "\n… 另有 %d 个节点", len(e.task.Clients)-limit)
		}
	}
	return builder.String()
}

func formatClientProgress(option ClientOption, progress *clientProgress) string {
	if progress == nil {
		progress = &clientProgress{name: option.Name, status: "queued"}
	}
	name := sanitizeTelegramLabel(option.Name, 28)
	if name == "" {
		name = "未命名节点"
	}
	icon := "○"
	switch progress.status {
	case "running", "assigned":
		icon = "•"
	case "completed":
		icon = "✓"
	case "failed":
		icon = "!"
	case "canceled":
		icon = "×"
	}
	detail := progress.phase
	if detail == "" {
		detail = telegramProgressStatus(progress.status)
	}
	if progress.total > 0 {
		detail += fmt.Sprintf(" %d/%d", min(max(progress.current, 0), progress.total), progress.total)
	}
	if progress.rateBPS > 0 {
		detail += fmt.Sprintf(" · %.2f Mbps", progress.rateBPS/1_000_000)
	}
	return fmt.Sprintf("%s %s · %s", icon, name, detail)
}

func terminalTargetStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "canceled"
}

func telegramProgressStatus(status string) string {
	switch status {
	case "queued":
		return "等待 Client"
	case "assigned":
		return "已下发"
	case "running":
		return "测速中"
	case "completed":
		return "已完成"
	case "partial":
		return "部分完成"
	case "failed":
		return "失败"
	case "canceled":
		return "已取消"
	default:
		if strings.TrimSpace(status) == "" {
			return "准备中"
		}
		return status
	}
}
