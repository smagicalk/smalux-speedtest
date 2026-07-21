package telegrambot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type sentMessage struct {
	chatID    int64
	messageID int64
	text      string
	replyTo   int64
	markup    *InlineKeyboardMarkup
}

type sentPhoto struct {
	chatID            int64
	filename, caption string
	data              []byte
	replyTo           int64
}

type fakeAPI struct {
	mu          sync.Mutex
	batches     [][]Update
	offsets     []int64
	messages    []sentMessage
	photos      []sentPhoto
	photoSignal chan struct{}
	commands    []BotCommand
	edits       []EditMessageRequest
	callbacks   []string
	deleted     []int64
}

func newFakeAPI(updates []Update) *fakeAPI {
	batches := [][]Update(nil)
	if updates != nil {
		batches = [][]Update{updates}
	}
	return &fakeAPI{batches: batches, photoSignal: make(chan struct{}, 16)}
}

func (a *fakeAPI) GetUpdates(ctx context.Context, offset int64, _ time.Duration) ([]Update, error) {
	a.mu.Lock()
	a.offsets = append(a.offsets, offset)
	if len(a.batches) > 0 {
		batch := a.batches[0]
		a.batches = a.batches[1:]
		a.mu.Unlock()
		return batch, nil
	}
	a.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (a *fakeAPI) SetMyCommands(_ context.Context, commands []BotCommand) error {
	a.mu.Lock()
	a.commands = append([]BotCommand(nil), commands...)
	a.mu.Unlock()
	return nil
}

func (a *fakeAPI) SendMessage(_ context.Context, request MessageRequest) (SentMessage, error) {
	a.mu.Lock()
	messageID := int64(len(a.messages) + 100)
	replyTo := int64(0)
	if request.ReplyParameters != nil {
		replyTo = request.ReplyParameters.MessageID
	}
	a.messages = append(a.messages, sentMessage{chatID: request.ChatID, messageID: messageID, text: request.Text, replyTo: replyTo, markup: request.ReplyMarkup})
	a.mu.Unlock()
	return SentMessage{MessageID: messageID}, nil
}

func (a *fakeAPI) EditMessageText(_ context.Context, request EditMessageRequest) error {
	a.mu.Lock()
	a.edits = append(a.edits, request)
	a.mu.Unlock()
	return nil
}

func (a *fakeAPI) AnswerCallbackQuery(_ context.Context, callbackQueryID, _ string) error {
	a.mu.Lock()
	a.callbacks = append(a.callbacks, callbackQueryID)
	a.mu.Unlock()
	return nil
}

func (a *fakeAPI) DeleteMessage(_ context.Context, _ int64, messageID int64) error {
	a.mu.Lock()
	a.deleted = append(a.deleted, messageID)
	a.mu.Unlock()
	return nil
}

func (a *fakeAPI) SendPhoto(_ context.Context, request PhotoRequest) error {
	a.mu.Lock()
	replyTo := int64(0)
	if request.ReplyParameters != nil {
		replyTo = request.ReplyParameters.MessageID
	}
	a.photos = append(a.photos, sentPhoto{chatID: request.ChatID, filename: request.Filename, caption: request.Caption, data: append([]byte(nil), request.Data...), replyTo: replyTo})
	a.mu.Unlock()
	a.photoSignal <- struct{}{}
	return nil
}

func (a *fakeAPI) snapshot() ([]sentMessage, []sentPhoto, []int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sentMessage(nil), a.messages...), append([]sentPhoto(nil), a.photos...), append([]int64(nil), a.offsets...)
}

func (a *fakeAPI) editsSnapshot() []EditMessageRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]EditMessageRequest(nil), a.edits...)
}

func (a *fakeAPI) commandsSnapshot() []BotCommand {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]BotCommand(nil), a.commands...)
}

type fakeAuthorization struct {
	mu     sync.Mutex
	users  map[int64]AuthorizedUser
	owners map[int64]bool
}

func newFakeAuthorization(owner int64, allowed ...int64) *fakeAuthorization {
	a := &fakeAuthorization{users: make(map[int64]AuthorizedUser), owners: map[int64]bool{owner: true}}
	a.users[owner] = AuthorizedUser{TelegramID: owner, Owner: true}
	for _, id := range allowed {
		a.users[id] = AuthorizedUser{TelegramID: id}
	}
	return a
}

func (a *fakeAuthorization) IsAuthorized(_ context.Context, id int64) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.users[id]
	return ok, nil
}

func (a *fakeAuthorization) IsOwner(_ context.Context, id int64) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.owners[id], nil
}

func (a *fakeAuthorization) AuthorizeUser(_ context.Context, user AuthorizedUser) error {
	a.mu.Lock()
	a.users[user.TelegramID] = user
	a.mu.Unlock()
	return nil
}

func (a *fakeAuthorization) RevokeUser(_ context.Context, id int64) error {
	a.mu.Lock()
	delete(a.users, id)
	a.mu.Unlock()
	return nil
}

func (a *fakeAuthorization) ListUsers(context.Context) ([]AuthorizedUser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	users := make([]AuthorizedUser, 0, len(a.users))
	for _, user := range a.users {
		users = append(users, user)
	}
	return users, nil
}

type fakeRunner struct {
	mu          sync.Mutex
	requests    []TaskRequest
	completion  Completion
	wait        chan Completion
	submitErr   error
	submitStart chan struct{}
	submitGate  chan struct{}
	submitOnce  sync.Once
	canceled    []string
	cancelCalls int
	cancelStart chan struct{}
	cancelGate  chan struct{}
	cancelOnce  sync.Once
	clients     []ClientOption
}

func (r *fakeRunner) ListAvailableClients(context.Context) ([]ClientOption, error) {
	if len(r.clients) == 0 {
		return []ClientOption{{ID: "client-1", Name: "client-1"}}, nil
	}
	return append([]ClientOption(nil), r.clients...), nil
}

func (r *fakeRunner) Submit(ctx context.Context, request TaskRequest) (Task, error) {
	if r.submitStart != nil {
		r.submitOnce.Do(func() { close(r.submitStart) })
	}
	if r.submitGate != nil {
		select {
		case <-r.submitGate:
		case <-ctx.Done():
			return Task{}, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.submitErr != nil {
		return Task{}, r.submitErr
	}
	r.requests = append(r.requests, request)
	clients := r.clients
	if len(clients) == 0 {
		clients = []ClientOption{{ID: "client-1", Name: "client-1"}}
	}
	if len(request.ClientIDs) > 0 {
		byID := make(map[string]ClientOption, len(clients))
		for _, client := range clients {
			byID[client.ID] = client
		}
		selected := make([]ClientOption, 0, len(request.ClientIDs))
		for _, id := range request.ClientIDs {
			if client, ok := byID[id]; ok {
				selected = append(selected, client)
			}
		}
		clients = selected
	}
	return Task{ID: fmt.Sprintf("task-%d", len(r.requests)), ClientCount: len(clients), ProxyCount: 1, TopN: max(request.TopN, 1), Clients: append([]ClientOption(nil), clients...)}, nil
}

func (r *fakeRunner) Wait(ctx context.Context, taskID string) (Completion, error) {
	if r.wait != nil {
		select {
		case completion := <-r.wait:
			completion.TaskID = taskID
			return completion, nil
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		}
	}
	completion := r.completion
	completion.TaskID = taskID
	return completion, nil
}

func (r *fakeRunner) Cancel(ctx context.Context, taskID string) error {
	r.mu.Lock()
	r.cancelCalls++
	r.mu.Unlock()
	if r.cancelStart != nil {
		r.cancelOnce.Do(func() { close(r.cancelStart) })
	}
	if r.cancelGate != nil {
		select {
		case <-r.cancelGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.canceled = append(r.canceled, taskID)
	r.mu.Unlock()
	if r.wait != nil {
		r.wait <- Completion{TaskID: taskID, Status: "canceled"}
	}
	return nil
}

func (r *fakeRunner) requestsSnapshot() []TaskRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TaskRequest(nil), r.requests...)
}

func (r *fakeRunner) canceledSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.canceled...)
}

func (r *fakeRunner) cancelCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelCalls
}

type fakeRenderer struct {
	mu          sync.Mutex
	completions []Completion
	image       Image
}

func (r *fakeRenderer) Render(_ context.Context, completion Completion) (Image, error) {
	r.mu.Lock()
	r.completions = append(r.completions, completion)
	r.mu.Unlock()
	return r.image, nil
}

func privateUpdate(updateID, userID int64, text string) Update {
	return Update{UpdateID: updateID, Message: &Message{
		MessageID: updateID, From: &User{ID: userID}, Chat: Chat{ID: userID, Type: "private"}, Text: text,
	}}
}

func containsMessage(messages []sentMessage, fragment string) bool {
	for _, message := range messages {
		if strings.Contains(message.text, fragment) {
			return true
		}
	}
	return false
}

func containsChat(messages []sentMessage, chatID int64) bool {
	for _, message := range messages {
		if message.chatID == chatID {
			return true
		}
	}
	return false
}

func waitForMessage(t *testing.T, api *fakeAPI, fragment string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		messages, _, _ := api.snapshot()
		if containsMessage(messages, fragment) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for message %q: %+v", fragment, messages)
		}
		time.Sleep(time.Millisecond)
	}
}
