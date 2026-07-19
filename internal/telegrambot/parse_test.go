package telegrambot

import "testing"

func TestParseSubmission(t *testing.T) {
	tests := []struct {
		name, text, source, subscription string
		wantError                        bool
	}{
		{name: "plain proxy", text: "ss://secret", source: "ss://secret"},
		{name: "plain HTTP remains proxy source", text: "http://user:pass@proxy.example:8080", source: "http://user:pass@proxy.example:8080"},
		{name: "batch source", text: "ss://one\nvless://two", source: "ss://one\nvless://two"},
		{name: "explicit test", text: "/test http://user:pass@proxy.example:8080", source: "http://user:pass@proxy.example:8080"},
		{name: "explicit subscription", text: "/sub https://example.com/sub", subscription: "https://example.com/sub"},
		{name: "command mention", text: "/speedtest@smalux_bot ss://node", source: "ss://node"},
		{name: "invalid subscription", text: "/sub file:///tmp/sub", wantError: true},
		{name: "unknown command", text: "/unknown", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, payload, isCommand := splitCommand(test.text)
			source, subscription, err := parseSubmission(command, payload, isCommand, test.text)
			if (err != nil) != test.wantError || source != test.source || subscription != test.subscription {
				t.Fatalf("parseSubmission(%q) = source %q, subscription %q, error %v", test.text, source, subscription, err)
			}
		})
	}
}
