package serverapp

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

const telegramCipherVersion = byte(1)

var telegramCipherAAD = []byte("smalux-speedtest/telegram-token/v1")

// loadOrCreateTelegramKey keeps the reversible key outside SQLite. The file is
// intentionally adjacent to the database so service ownership and backup policy are
// explicit, but logs and API responses never expose its path.
func loadOrCreateTelegramKey(databasePath string) ([chacha20poly1305.KeySize]byte, error) {
	var key [chacha20poly1305.KeySize]byte
	if databasePath == ":memory:" || databasePath == "" {
		_, err := io.ReadFull(rand.Reader, key[:])
		return key, err
	}
	path := databasePath + ".bot-key"
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return key, errors.New("telegram key file must be a private regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) != len(key) {
			return key, errors.New("telegram key file is invalid")
		}
		copy(key[:], data)
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return key, err
	}
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return key, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return [chacha20poly1305.KeySize]byte{}, err
	}
	_, writeErr := file.Write(key[:])
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return [chacha20poly1305.KeySize]byte{}, errors.New("write telegram key file")
	}
	return key, nil
}

func encryptTelegramToken(key [chacha20poly1305.KeySize]byte, token string) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	result := make([]byte, 1, 1+len(nonce)+len(token)+aead.Overhead())
	result[0] = telegramCipherVersion
	result = append(result, nonce...)
	return aead.Seal(result, nonce, []byte(token), telegramCipherAAD), nil
}

func decryptTelegramToken(key [chacha20poly1305.KeySize]byte, ciphertext []byte) (string, error) {
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return "", err
	}
	minimum := 1 + aead.NonceSize() + aead.Overhead()
	if len(ciphertext) < minimum || ciphertext[0] != telegramCipherVersion {
		return "", errors.New("encrypted telegram token is invalid")
	}
	nonce := ciphertext[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, ciphertext[1+aead.NonceSize():], telegramCipherAAD)
	if err != nil {
		return "", fmt.Errorf("decrypt telegram token: %w", err)
	}
	return string(plaintext), nil
}
