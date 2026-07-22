package serverapp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTelegramTokenEncryptionAndPrivateKeyFile(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "service.db")
	key, err := loadOrCreateTelegramKey(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(databasePath + ".bot-key")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("Bot key permissions = %v", info.Mode().Perm())
	}
	const token = "123456:secret-bot-token"
	cipher, err := encryptTelegramToken(key, token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte(token)) {
		t.Fatal("encrypted Bot configuration contains plaintext token")
	}
	plaintext, err := decryptTelegramToken(key, cipher)
	if err != nil || plaintext != token {
		t.Fatalf("decrypted token = %q, err=%v", plaintext, err)
	}
	cipher[len(cipher)-1] ^= 1
	if _, err := decryptTelegramToken(key, cipher); err == nil {
		t.Fatal("tampered Bot token ciphertext was accepted")
	}
}

func TestTelegramKeyFileValidation(t *testing.T) {
	t.Run("reloads existing key", func(t *testing.T) {
		databasePath := filepath.Join(t.TempDir(), "service.db")
		first, err := loadOrCreateTelegramKey(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		second, err := loadOrCreateTelegramKey(databasePath)
		if err != nil || first != second {
			t.Fatalf("reloaded key matches=%t err=%v", first == second, err)
		}
	})
	t.Run("rejects public permissions", func(t *testing.T) {
		databasePath := filepath.Join(t.TempDir(), "service.db")
		if _, err := loadOrCreateTelegramKey(databasePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(databasePath+".bot-key", 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadOrCreateTelegramKey(databasePath); err == nil {
			t.Fatal("publicly readable Telegram key was accepted")
		}
	})
	t.Run("rejects invalid length", func(t *testing.T) {
		databasePath := filepath.Join(t.TempDir(), "service.db")
		if err := os.WriteFile(databasePath+".bot-key", []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadOrCreateTelegramKey(databasePath); err == nil {
			t.Fatal("short Telegram key was accepted")
		}
	})
}

func TestTelegramTokenCipherRejectsMalformedData(t *testing.T) {
	key, err := loadOrCreateTelegramKey(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	valid, err := encryptTelegramToken(key, "123456:test")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"empty":         nil,
		"truncated":     append([]byte(nil), valid[:len(valid)-1]...),
		"wrong version": append([]byte{telegramCipherVersion + 1}, valid[1:]...),
	}
	for name, cipher := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decryptTelegramToken(key, cipher); err == nil {
				t.Fatal("malformed ciphertext was accepted")
			}
		})
	}
}
