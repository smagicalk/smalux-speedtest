package serverapp

import "testing"

func TestTelegramBotIDFromToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
		want  int64
	}{
		{name: "standard token", token: "123456:first-secret", want: 123456},
		{name: "rotated secret", token: "123456:rotated-secret", want: 123456},
		{name: "surrounding whitespace", token: " 654321:other-secret ", want: 654321},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := telegramBotIDFromToken(tt.token)
			if err != nil {
				t.Fatalf("telegramBotIDFromToken() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("telegramBotIDFromToken() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTelegramBotIDFromTokenRejectsInvalidToken(t *testing.T) {
	t.Parallel()
	for _, token := range []string{
		"",
		"no-colon",
		"abc:secret",
		"0:secret",
		"-1:secret",
		"123456:",
	} {
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			if _, err := telegramBotIDFromToken(token); err == nil {
				t.Fatalf("telegramBotIDFromToken(%q) unexpectedly succeeded", token)
			}
		})
	}
}
