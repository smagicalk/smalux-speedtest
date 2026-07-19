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
	chatID int64
	text   string
}

type sentPhoto struct {
	chatID            int64
	filename, caption string
	data              []byte
}

type fakeAPI struct {
	mu          sync.Mutex
	batches     [][]Update
	offsets     []int64
	messages    []sentMessage
	photos      []sentPhoto
	photoSignal chan struct{}
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

func (a *fakeAPI) SendMessage(_ context.Context, chatID int64, text string) error {
	a.mu.Lock()
	a.messages = append(a.messages, sentMessage{chatID: chatID, text: text})
	a.mu.Unlock()
	return nil
}

func (a *fakeAPI) SendPhoto(_ context.Context, chatID int64, filename, caption string, data []byte) error {
	a.mu.Lock()
	a.photos = append(a.photos, sentPhoto{chatID: chatID, filename: filename, caption: caption, data: append([]byte(nil), data...)})
	a.mu.Unlock()
	a.photoSignal <- struct{}{}
	return nil
}

func (a *fakeAPI) snapshot() ([]sentMessage, []sentPhoto, []int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sentMessage(nil), a.messages...), append([]sentPhoto(nil), a.photos...), append([]int64(nil), a.offsets...)
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
	return Task{ID: fmt.Sprintf("task-%d", len(r.requests))}, nil
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
