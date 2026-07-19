package serverapp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/wire"
)

type peer struct {
	client store.Client
	conn   *websocket.Conn
	write  sync.Mutex
}

func (p *peer) send(ctx context.Context, envelope wire.Envelope) error {
	p.write.Lock()
	defer p.write.Unlock()
	return wsjson.Write(ctx, p.conn, envelope)
}

type runtimeTask struct {
	assignment model.Assignment
	targets    map[string]string
	expires    *time.Timer
}

type taskEvent struct {
	Type     string             `json:"type"`
	Progress *model.Progress    `json:"progress,omitempty"`
	Result   *model.SpeedResult `json:"result,omitempty"`
	Status   string             `json:"status,omitempty"`
	Message  string             `json:"message,omitempty"`
}

type Hub struct {
	store *store.Store
	log   *slog.Logger

	mu          sync.RWMutex
	peers       map[string]*peer
	tasks       map[string]*runtimeTask
	subscribers map[string]map[chan taskEvent]struct{}
}

func NewHub(store *store.Store, logger *slog.Logger) *Hub {
	return &Hub{
		store:       store,
		log:         logger,
		peers:       make(map[string]*peer),
		tasks:       make(map[string]*runtimeTask),
		subscribers: make(map[string]map[chan taskEvent]struct{}),
	}
}

func (h *Hub) Online(clientID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.peers[clientID]
	return ok
}

func (h *Hub) AddTask(assignment model.Assignment, clientIDs []string) {
	task := &runtimeTask{assignment: assignment, targets: make(map[string]string)}
	for _, id := range clientIDs {
		task.targets[id] = "queued"
	}
	task.expires = time.AfterFunc(10*time.Minute, func() { h.expireTask(assignment.TaskID) })
	h.mu.Lock()
	h.tasks[assignment.TaskID] = task
	h.mu.Unlock()
	for _, id := range clientIDs {
		h.dispatch(assignment.TaskID, id)
	}
}

func (h *Hub) CancelTask(ctx context.Context, taskID string) error {
	h.mu.Lock()
	task, ok := h.tasks[taskID]
	if !ok {
		h.mu.Unlock()
		return errors.New("task is not active")
	}
	if task.expires != nil {
		task.expires.Stop()
	}
	var peers []*peer
	for clientID, status := range task.targets {
		if status != "completed" && status != "failed" {
			task.targets[clientID] = "canceled"
			if connected := h.peers[clientID]; connected != nil {
				peers = append(peers, connected)
			}
		}
	}
	delete(h.tasks, taskID)
	h.mu.Unlock()

	message, _ := wire.New(wire.TypeTaskCancel, taskID, model.Ack{TaskID: taskID})
	for _, connected := range peers {
		sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = connected.send(sendCtx, message)
		cancel()
	}
	_ = h.store.SetTaskStatus(ctx, taskID, "canceled", "canceled by administrator")
	h.publish(taskID, taskEvent{Type: "status", Status: "canceled"})
	return nil
}

func (h *Hub) ServeWebSocket(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r.Header.Get("Authorization"))
	client, err := h.store.AuthenticateClient(r.Context(), token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	conn.SetReadLimit(2 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "connection closed")

	helloCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	var first wire.Envelope
	err = wsjson.Read(helloCtx, conn, &first)
	cancel()
	if err != nil || first.Version != model.ProtocolVersion || first.Type != wire.TypeHello {
		conn.Close(websocket.StatusPolicyViolation, "client.hello required")
		return
	}
	hello, err := wire.Decode[model.Hello](first)
	if err != nil || strings.TrimSpace(hello.Name) == "" {
		conn.Close(websocket.StatusPolicyViolation, "invalid client.hello")
		return
	}
	if err := h.store.UpdateClientHello(r.Context(), client.ID, hello); err != nil {
		conn.Close(websocket.StatusInternalError, "failed to register client")
		return
	}
	client.Name, client.Version, client.OS, client.Arch, client.Labels = hello.Name, hello.Version, hello.OS, hello.Arch, hello.Labels
	connected := &peer{client: client, conn: conn}
	h.register(connected)
	defer h.unregister(connected)

	welcome, _ := wire.New(wire.TypeWelcome, "", model.Welcome{ClientID: client.ID})
	writeCtx, writeCancel := context.WithTimeout(r.Context(), 5*time.Second)
	if err := connected.send(writeCtx, welcome); err != nil {
		writeCancel()
		return
	}
	writeCancel()
	h.dispatchQueued(client.ID)
	h.log.Info("client connected", "client_id", client.ID, "name", hello.Name, "remote", r.RemoteAddr)

	for {
		var message wire.Envelope
		if err := wsjson.Read(r.Context(), conn, &message); err != nil {
			return
		}
		if message.Version != model.ProtocolVersion {
			continue
		}
		h.handleMessage(r.Context(), connected, message)
	}
}

func (h *Hub) register(connected *peer) {
	h.mu.Lock()
	previous := h.peers[connected.client.ID]
	var requeued []string
	if previous != nil {
		for taskID, task := range h.tasks {
			if task.targets[connected.client.ID] == "running" {
				task.targets[connected.client.ID] = "queued"
				requeued = append(requeued, taskID)
			}
		}
	}
	h.peers[connected.client.ID] = connected
	h.mu.Unlock()
	for _, taskID := range requeued {
		_ = h.store.SetTargetStatus(context.Background(), taskID, connected.client.ID, "queued", "client connection replaced")
	}
	if previous != nil {
		previous.conn.Close(websocket.StatusPolicyViolation, "replaced by a new connection")
	}
}

func (h *Hub) RevokeClient(ctx context.Context, clientID string) {
	h.mu.Lock()
	connected := h.peers[clientID]
	if connected != nil {
		delete(h.peers, clientID)
	}
	var activeTasks []string
	for taskID, task := range h.tasks {
		status := task.targets[clientID]
		if status == "queued" || status == "running" {
			activeTasks = append(activeTasks, taskID)
		}
	}
	h.mu.Unlock()
	if connected != nil {
		connected.conn.Close(websocket.StatusPolicyViolation, "client token revoked")
	}
	for _, taskID := range activeTasks {
		h.finishTarget(ctx, taskID, clientID, "failed", "client token revoked")
	}
}

func (h *Hub) unregister(connected *peer) {
	h.mu.Lock()
	var requeued []string
	if h.peers[connected.client.ID] == connected {
		delete(h.peers, connected.client.ID)
		for taskID, task := range h.tasks {
			if task.targets[connected.client.ID] == "running" {
				task.targets[connected.client.ID] = "queued"
				requeued = append(requeued, taskID)
			}
		}
	}
	h.mu.Unlock()
	for _, taskID := range requeued {
		_ = h.store.SetTargetStatus(context.Background(), taskID, connected.client.ID, "queued", "client disconnected")
	}
	_ = h.store.TouchClient(context.Background(), connected.client.ID)
	h.log.Info("client disconnected", "client_id", connected.client.ID)
}

func (h *Hub) dispatchQueued(clientID string) {
	h.mu.RLock()
	var taskIDs []string
	for taskID, task := range h.tasks {
		if task.targets[clientID] == "queued" {
			taskIDs = append(taskIDs, taskID)
		}
	}
	h.mu.RUnlock()
	for _, taskID := range taskIDs {
		h.dispatch(taskID, clientID)
	}
}

func (h *Hub) dispatch(taskID, clientID string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	connected := h.peers[clientID]
	if task == nil || connected == nil || task.targets[clientID] != "queued" {
		h.mu.RUnlock()
		return
	}
	assignment := task.assignment
	h.mu.RUnlock()
	message, err := wire.New(wire.TypeTaskAssign, taskID, assignment)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = connected.send(ctx, message)
	cancel()
	if err != nil {
		h.log.Warn("task dispatch failed", "task_id", taskID, "client_id", clientID, "error", err)
	}
}

func (h *Hub) handleMessage(ctx context.Context, connected *peer, message wire.Envelope) {
	switch message.Type {
	case wire.TypePing:
		response, _ := wire.New(wire.TypePong, "", struct{}{})
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = connected.send(writeCtx, response)
		cancel()
		_ = h.store.TouchClient(ctx, connected.client.ID)
	case wire.TypeTaskAck:
		h.setTargetRunning(ctx, message.TaskID, connected.client.ID)
	case wire.TypeTaskProgress:
		progress, err := wire.Decode[model.Progress](message)
		if err == nil && progress.TaskID == message.TaskID {
			h.publish(message.TaskID, taskEvent{Type: "progress", Progress: &progress})
		}
	case wire.TypeTaskResult:
		result, err := wire.Decode[model.SpeedResult](message)
		if err == nil && result.TaskID == message.TaskID && h.acceptsResult(message.TaskID, connected.client.ID) {
			result.ClientID = connected.client.ID
			if err := h.store.SaveResult(ctx, result); err == nil {
				h.publish(message.TaskID, taskEvent{Type: "result", Result: &result})
			}
		}
	case wire.TypeTaskComplete:
		h.finishTarget(ctx, message.TaskID, connected.client.ID, "completed", "")
	case wire.TypeTaskFailed:
		failure, err := wire.Decode[model.Failure](message)
		if err == nil {
			h.finishTarget(ctx, message.TaskID, connected.client.ID, "failed", failure.Error)
		}
	}
}

func (h *Hub) setTargetRunning(ctx context.Context, taskID, clientID string) {
	h.mu.Lock()
	task := h.tasks[taskID]
	transitioned := false
	if task != nil && task.targets[clientID] == "queued" {
		task.targets[clientID] = "running"
		transitioned = true
	}
	h.mu.Unlock()
	if !transitioned {
		return
	}
	_ = h.store.SetTargetStatus(ctx, taskID, clientID, "running", "")
	_ = h.store.SetTaskStatus(ctx, taskID, "running", "")
	h.publish(taskID, taskEvent{Type: "status", Status: "running"})
}

func (h *Hub) acceptsResult(taskID, clientID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	task := h.tasks[taskID]
	return task != nil && task.targets[clientID] == "running"
}

func (h *Hub) finishTarget(ctx context.Context, taskID, clientID, status, detail string) {
	h.mu.Lock()
	task := h.tasks[taskID]
	if task == nil {
		h.mu.Unlock()
		return
	}
	current := task.targets[clientID]
	if current == "completed" || current == "failed" || current == "canceled" {
		h.mu.Unlock()
		return
	}
	task.targets[clientID] = status
	h.mu.Unlock()
	_ = h.store.SetTargetStatus(ctx, taskID, clientID, status, detail)
	h.aggregate(ctx, taskID)
}

func (h *Hub) aggregate(ctx context.Context, taskID string) {
	completed, failed, canceled, total, err := h.store.TargetSummary(ctx, taskID)
	if err != nil || completed+failed+canceled < total {
		return
	}
	status := "completed"
	detail := ""
	if canceled == total {
		status = "canceled"
	} else if failed == total {
		status = "failed"
		detail = "all clients failed"
	} else if failed > 0 || canceled > 0 {
		status = "partial"
		detail = "some clients did not complete"
	}
	_ = h.store.SetTaskStatus(ctx, taskID, status, detail)
	h.mu.Lock()
	if task := h.tasks[taskID]; task != nil {
		if task.expires != nil {
			task.expires.Stop()
		}
		task.assignment.Proxies = nil
		delete(h.tasks, taskID)
	}
	h.mu.Unlock()
	h.publish(taskID, taskEvent{Type: "status", Status: status, Message: detail})
}

func (h *Hub) expireTask(taskID string) {
	h.mu.RLock()
	task := h.tasks[taskID]
	if task == nil {
		h.mu.RUnlock()
		return
	}
	var pending []string
	for clientID, status := range task.targets {
		if status == "queued" || status == "running" {
			pending = append(pending, clientID)
		}
	}
	h.mu.RUnlock()
	for _, clientID := range pending {
		h.finishTarget(context.Background(), taskID, clientID, "failed", "task expired after 10 minutes")
	}
}

func (h *Hub) Subscribe(taskID string) (<-chan taskEvent, func()) {
	channel := make(chan taskEvent, 32)
	h.mu.Lock()
	if h.subscribers[taskID] == nil {
		h.subscribers[taskID] = make(map[chan taskEvent]struct{})
	}
	h.subscribers[taskID][channel] = struct{}{}
	h.mu.Unlock()
	return channel, func() {
		h.mu.Lock()
		delete(h.subscribers[taskID], channel)
		if len(h.subscribers[taskID]) == 0 {
			delete(h.subscribers, taskID)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) publish(taskID string, event taskEvent) {
	h.mu.RLock()
	channels := make([]chan taskEvent, 0, len(h.subscribers[taskID]))
	for channel := range h.subscribers[taskID] {
		channels = append(channels, channel)
	}
	h.mu.RUnlock()
	for _, channel := range channels {
		select {
		case channel <- event:
		default:
		}
	}
}

func bearerToken(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func eventJSON(event taskEvent) []byte {
	value, _ := json.Marshal(event)
	return value
}
