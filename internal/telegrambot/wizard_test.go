package telegrambot

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestWizardConfiguresTaskAndRepliesToSource(t *testing.T) {
	api := newFakeAPI(nil)
	runner := &fakeRunner{
		completion: Completion{Status: "completed"},
		clients:    []ClientOption{{ID: "client-a", Name: "Shanghai"}, {ID: "client-b", Name: "Tokyo"}},
	}
	renderer := &fakeRenderer{image: Image{Filename: "result.png", Data: []byte("PNG")}}
	bot, err := New(api, newFakeAuthorization(9, 1), runner, renderer, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}

	bot.handleUpdate(t.Context(), privateUpdate(1, 1, "/start"))
	waitForMessage(t, api, StartText)
	messages, photos, _ := api.snapshot()
	if len(messages) != 1 || messages[0].markup == nil || messages[0].markup.InlineKeyboard[0][0].CallbackData != "menu:test" {
		t.Fatalf("start menu missing: %+v", messages)
	}
	menuMessage := &Message{MessageID: messages[0].messageID, Chat: Chat{ID: 1, Type: "private"}}
	bot.handleUpdate(t.Context(), Update{UpdateID: 2, CallbackQuery: &CallbackQuery{ID: "cb-start", From: User{ID: 1}, Message: menuMessage, Data: "menu:test"}})
	bot.handleUpdate(t.Context(), privateUpdate(3, 1, "ss://node"))
	state := bot.loadWizard(1)
	if state == nil || state.stage != wizardClients || state.requestMessageID != 3 || state.controlMessageID == 0 {
		t.Fatalf("source was not captured by wizard: %+v", state)
	}
	controlMessage := &Message{MessageID: state.controlMessageID, Chat: Chat{ID: 1, Type: "private"}}
	bot.handleUpdate(t.Context(), Update{UpdateID: 4, CallbackQuery: &CallbackQuery{ID: "cb-client", From: User{ID: 1}, Message: controlMessage, Data: wizardData(state.id, "client", "1")}})
	bot.handleUpdate(t.Context(), Update{UpdateID: 5, CallbackQuery: &CallbackQuery{ID: "cb-client-next", From: User{ID: 1}, Message: controlMessage, Data: wizardData(state.id, "client-next")}})
	bot.handleUpdate(t.Context(), Update{UpdateID: 6, CallbackQuery: &CallbackQuery{ID: "cb-candidate", From: User{ID: 1}, Message: controlMessage, Data: wizardData(state.id, "candidate", "10")}})
	bot.handleUpdate(t.Context(), Update{UpdateID: 7, CallbackQuery: &CallbackQuery{ID: "cb-top", From: User{ID: 1}, Message: controlMessage, Data: wizardData(state.id, "top", "3")}})
	bot.handleUpdate(t.Context(), Update{UpdateID: 8, CallbackQuery: &CallbackQuery{ID: "cb-threads", From: User{ID: 1}, Message: controlMessage, Data: wizardData(state.id, "threads", "8")}})
	bot.handleUpdate(t.Context(), Update{UpdateID: 9, CallbackQuery: &CallbackQuery{ID: "cb-confirm", From: User{ID: 1}, Message: controlMessage, Data: wizardData(state.id, "confirm")}})

	select {
	case <-api.photoSignal:
	case <-time.After(2 * time.Second):
		t.Fatal("wizard task did not return a photo")
	}
	bot.wg.Wait()
	requests := runner.requestsSnapshot()
	if len(requests) != 1 || requests[0].CandidateCount != 10 || requests[0].TopN != 3 || requests[0].Threads != 8 || len(requests[0].ClientIDs) != 1 || requests[0].ClientIDs[0] != "client-a" {
		t.Fatalf("wizard parameters were not submitted: %+v", requests)
	}
	messages, photos, _ = api.snapshot()
	if len(messages) == 0 || messages[0].replyTo != 0 {
		t.Fatalf("menu unexpectedly replied to a source: %+v", messages)
	}
	if len(photos) != 1 || photos[0].replyTo != 3 {
		t.Fatalf("result photo did not quote source request: %+v", photos)
	}
	if len(api.editsSnapshot()) == 0 {
		t.Fatal("wizard/progress did not edit a message")
	}
	api.mu.Lock()
	deleted := append([]int64(nil), api.deleted...)
	api.mu.Unlock()
	if len(deleted) < 2 || deleted[len(deleted)-1] != state.controlMessageID {
		t.Fatalf("temporary menu/progress messages were not removed: %v", deleted)
	}
}

func TestWizardClientPaginationAndSelection(t *testing.T) {
	clients := make([]ClientOption, 7)
	for index := range clients {
		clients[index] = ClientOption{ID: fmt.Sprintf("client-%d", index+1), Name: fmt.Sprintf("Node %d", index+1)}
	}
	api := newFakeAPI(nil)
	bot, err := New(api, newFakeAuthorization(9, 1), &fakeRunner{clients: clients}, &fakeRenderer{}, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	state := bot.newWizard(1)
	state.stage, state.source, state.requestMessageID = wizardAwaitSource, []byte("ss://node"), 7
	bot.beginClientSelection(t.Context(), 1, state)
	state = bot.loadWizard(1)
	if state == nil || clientPageCount(state) != 2 || len(clientKeyboard(state).InlineKeyboard) != 9 {
		t.Fatalf("unexpected first client page: state=%+v keyboard=%+v", state, clientKeyboard(state))
	}
	message := &Message{MessageID: state.controlMessageID, Chat: Chat{ID: 1, Type: "private"}}
	callback := func(id, action string, values ...string) {
		bot.handleUpdate(t.Context(), Update{CallbackQuery: &CallbackQuery{ID: id, From: User{ID: 1}, Message: message, Data: wizardData(state.id, action, values...)}})
	}
	callback("page-2", "client-page", "1")
	if bot.loadWizard(1).clientPage != 1 {
		t.Fatal("client pagination did not advance")
	}
	callback("clear", "client-none")
	callback("empty-next", "client-next")
	if bot.loadWizard(1).stage != wizardClients {
		t.Fatal("empty client selection advanced the wizard")
	}
	callback("select-last", "client", "6")
	callback("next", "client-next")
	state = bot.loadWizard(1)
	if state.stage != wizardCandidates || len(selectedClientIDs(state)) != 1 || selectedClientIDs(state)[0] != "client-7" {
		t.Fatalf("selected clients were not retained: %+v", state)
	}
}

func TestWizardRejectsInvalidCallbackValues(t *testing.T) {
	api := newFakeAPI(nil)
	bot, err := New(api, newFakeAuthorization(9, 1), &fakeRunner{}, &fakeRenderer{}, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	state := bot.newWizard(1)
	state.stage = wizardCandidates
	bot.storeWizard(1, state)
	bot.handleUpdate(t.Context(), Update{UpdateID: 1, CallbackQuery: &CallbackQuery{ID: "bad", From: User{ID: 1}, Message: &Message{MessageID: 1, Chat: Chat{ID: 1, Type: "private"}}, Data: wizardData(state.id, "candidate", "999")}})
	if current := bot.loadWizard(1); current == nil || current.stage != wizardCandidates {
		t.Fatalf("invalid callback advanced wizard: %+v", current)
	}
}
