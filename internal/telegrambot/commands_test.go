package telegrambot

import (
	"io"
	"log/slog"
	"testing"
)

// TestOwnerAuthorizationCommands 验证普通用户不能管理名单，owner 可按回复授权并撤销。
func TestOwnerAuthorizationCommands(t *testing.T) {
	api := newFakeAPI(nil)
	authorization := newFakeAuthorization(9)
	bot, err := New(api, authorization, &fakeRunner{}, &fakeRenderer{}, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	bot.handleUpdate(t.Context(), privateUpdate(1, 2, "/authorize 42"))
	if allowed, _ := authorization.IsAuthorized(t.Context(), 42); allowed {
		t.Fatal("non-owner authorized a user")
	}
	reply := privateUpdate(2, 9, "/authorize").Message
	reply.ReplyToMessage = &Message{From: &User{ID: 42, Username: "worker", FirstName: "Speed", LastName: "Node"}}
	bot.handleUpdate(t.Context(), Update{UpdateID: 2, Message: reply})
	if allowed, _ := authorization.IsAuthorized(t.Context(), 42); !allowed {
		t.Fatal("owner did not authorize replied user")
	}
	bot.handleUpdate(t.Context(), privateUpdate(3, 9, "/users"))
	bot.handleUpdate(t.Context(), privateUpdate(4, 9, "/revoke 42"))
	if allowed, _ := authorization.IsAuthorized(t.Context(), 42); allowed {
		t.Fatal("owner did not revoke user")
	}
	messages, _, _ := api.snapshot()
	if !containsMessage(messages, "仅 Bot 所有者") || !containsMessage(messages, "42 @worker (Speed Node)") || !containsMessage(messages, "已撤销用户 42") {
		t.Fatalf("unexpected authorization replies: %+v", messages)
	}
}
