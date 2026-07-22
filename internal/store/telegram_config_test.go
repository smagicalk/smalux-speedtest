package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

func TestTelegramConfigLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "telegram-config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, _, err := database.LoadTelegramConfig(t.Context()); !errors.Is(err, ErrTelegramConfigMissing) {
		t.Fatalf("missing config error = %v", err)
	}
	cipher := []byte{1, 2, 3, 4}
	if err := database.SaveTelegramConfig(t.Context(), cipher, 123456, "smalux_bot", 998877, true); err != nil {
		t.Fatal(err)
	}
	config, storedCipher, err := database.LoadTelegramConfig(t.Context())
	if err != nil || config.BotID != 123456 || config.OwnerTelegramID != 998877 || !config.Enabled || !bytes.Equal(cipher, storedCipher) {
		t.Fatalf("stored config = %+v cipher=%v err=%v", config, storedCipher, err)
	}
	if err := database.SetTelegramEnabled(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	config, _, err = database.LoadTelegramConfig(t.Context())
	if err != nil || config.Enabled {
		t.Fatalf("disabled config = %+v err=%v", config, err)
	}
	if err := database.DeleteTelegramConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.LoadTelegramConfig(t.Context()); !errors.Is(err, ErrTelegramConfigMissing) {
		t.Fatalf("deleted config error = %v", err)
	}
}

func TestTelegramConfigRejectsInvalidWrites(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "telegram-invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	tests := []struct {
		name    string
		cipher  []byte
		botID   int64
		ownerID int64
	}{
		{name: "empty cipher", botID: 1, ownerID: 1},
		{name: "invalid bot", cipher: []byte{1}, ownerID: 1},
		{name: "invalid owner", cipher: []byte{1}, botID: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := database.SaveTelegramConfig(t.Context(), test.cipher, test.botID, "bot", test.ownerID, true); err == nil {
				t.Fatal("invalid Telegram configuration was saved")
			}
		})
	}
	if err := database.SetTelegramEnabled(t.Context(), true); !errors.Is(err, ErrTelegramConfigMissing) {
		t.Fatalf("enable missing configuration error=%v", err)
	}
}
