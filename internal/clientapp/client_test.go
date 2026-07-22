package clientapp

import "testing"

func TestNewValidatesWebSocketServerURL(t *testing.T) {
	valid := []string{
		"ws://127.0.0.1:8080/ws/client",
		"ws://[::1]:8080/ws/client",
		"ws://localhost:8080/ws/client",
		"ws://198.51.100.10:8080/ws/client",
		"wss://speed.example.com/ws/client",
	}
	for _, serverURL := range valid {
		if _, err := New(Config{ServerURL: serverURL, Token: "token", Name: "node"}); err != nil {
			t.Fatalf("valid server URL %q rejected: %v", serverURL, err)
		}
	}
	invalid := []string{
		"",
		"http://127.0.0.1:8080/ws/client",
		"ws:///ws/client",
		"ws://user:pass@example.com/ws",
		"wss://speed.example.com/ws/client?token=secret",
		"wss://speed.example.com/ws/client?",
		"wss://speed.example.com/ws/client#secret",
	}
	for _, serverURL := range invalid {
		if _, err := New(Config{ServerURL: serverURL, Token: "token", Name: "node"}); err == nil {
			t.Fatalf("invalid server URL %q was accepted", serverURL)
		}
	}
	if _, err := New(Config{ServerURL: valid[0], Token: "   ", Name: "node"}); err == nil {
		t.Fatal("blank client token was accepted")
	}
	if client, err := New(Config{ServerURL: valid[0], Token: "token"}); err != nil || client.config.Name != "smalux-client" {
		t.Fatalf("default client name = %q, err = %v", client.config.Name, err)
	}
}
