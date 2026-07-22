package serverapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/telegrambot"
)

const telegramBindingTTL = 5 * time.Minute

type telegramBindingChallenge struct {
	adminID   string
	token     string
	ownerID   int64
	botID     int64
	username  string
	code      string
	expiresAt time.Time
	attempts  int
}

type telegramSettingsView struct {
	Configured      bool   `json:"configured"`
	Enabled         bool   `json:"enabled"`
	Running         bool   `json:"running"`
	BotID           int64  `json:"bot_id,omitempty"`
	BotUsername     string `json:"bot_username,omitempty"`
	OwnerTelegramID int64  `json:"owner_telegram_id,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
}

func (a *App) telegramSettingsPage(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "settings.html", pageData{
		CSRF: session.csrf, CurrentAdminID: session.userID, Username: session.username, IsOwner: session.isOwner,
	})
}

func (a *App) getTelegramSettings(w http.ResponseWriter, r *http.Request) {
	config, _, err := a.store.LoadTelegramConfig(r.Context())
	if errors.Is(err, store.ErrTelegramConfigMissing) {
		writeJSON(w, http.StatusOK, telegramSettingsView{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("读取 Telegram 配置失败"))
		return
	}
	writeJSON(w, http.StatusOK, telegramSettingsView{
		Configured: true, Enabled: config.Enabled, Running: a.telegramIsRunning(), BotID: config.BotID,
		BotUsername: config.BotUsername, OwnerTelegramID: config.OwnerTelegramID, UpdatedAt: config.UpdatedAt,
	})
}

// beginTelegramBinding 验证 Token 的 getMe 身份，并向 owner ID 发送只有该 Telegram
// 账户能读取的一次性验证码。Token 在确认前仅保存在内存，五分钟后失效。
func (a *App) beginTelegramBinding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var request struct {
		Token   string `json:"token"`
		OwnerID int64  `json:"owner_id"`
	}
	if err := decodeJSON(r, &request); err != nil || request.OwnerID <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("请输入有效的 Bot Token 和 Telegram 用户 ID"))
		return
	}
	token := strings.TrimSpace(request.Token)
	botID, err := telegramBotIDFromToken(token)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("Bot Token 格式无效"))
		return
	}
	api, err := telegrambot.NewHTTPAPI(telegrambot.HTTPConfig{Token: token, BaseURL: a.config.TelegramAPIBaseURL, Client: a.config.TelegramHTTPClient})
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("Bot Token 格式无效"))
		return
	}
	verifyCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	identity, err := api.GetMe(verifyCtx)
	cancel()
	if err != nil || identity.ID != botID {
		writeError(w, http.StatusBadRequest, errors.New("Bot Token 验证失败"))
		return
	}
	code, err := telegramVerificationCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("生成验证码失败"))
		return
	}
	challengeID, err := telegramChallengeID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("生成绑定请求失败"))
		return
	}
	message := fmt.Sprintf("Smalux Speedtest 绑定验证码：%s\n验证码 5 分钟内有效。若不是你发起的绑定，请忽略。", code)
	sendCtx, sendCancel := context.WithTimeout(r.Context(), 15*time.Second)
	_, err = api.SendMessage(sendCtx, telegrambot.MessageRequest{ChatID: request.OwnerID, Text: message})
	sendCancel()
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("无法发送验证码，请先打开 Bot 并发送 /start"))
		return
	}
	session, _ := a.session(r)
	challenge := &telegramBindingChallenge{
		adminID: session.userID, token: token, ownerID: request.OwnerID, botID: identity.ID,
		username: identity.Username, code: code, expiresAt: time.Now().Add(telegramBindingTTL), attempts: 5,
	}
	a.telegramMu.Lock()
	for id, pending := range a.telegramBindings {
		if pending.adminID == session.userID || time.Now().After(pending.expiresAt) {
			delete(a.telegramBindings, id)
		}
	}
	a.telegramBindings[challengeID] = challenge
	a.telegramMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"challenge_id": challengeID, "bot_username": identity.Username,
		"expires_at": challenge.expiresAt.UTC().Format(time.RFC3339),
	})
}

func (a *App) confirmTelegramBinding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var request struct {
		ChallengeID string `json:"challenge_id"`
		Code        string `json:"code"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("绑定请求无效"))
		return
	}
	session, _ := a.session(r)
	a.telegramMu.Lock()
	challenge := a.telegramBindings[strings.TrimSpace(request.ChallengeID)]
	if challenge == nil || challenge.adminID != session.userID || time.Now().After(challenge.expiresAt) || challenge.attempts <= 0 {
		if challenge != nil {
			delete(a.telegramBindings, request.ChallengeID)
		}
		a.telegramMu.Unlock()
		writeError(w, http.StatusBadRequest, errors.New("验证码已失效，请重新验证"))
		return
	}
	challenge.attempts--
	valid := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(request.Code)), []byte(challenge.code)) == 1
	if !valid {
		a.telegramMu.Unlock()
		writeError(w, http.StatusBadRequest, errors.New("验证码错误"))
		return
	}
	delete(a.telegramBindings, request.ChallengeID)
	a.telegramMu.Unlock()

	key, err := a.telegramEncryptionKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("初始化 Bot 安全存储失败"))
		return
	}
	cipher, err := encryptTelegramToken(key, challenge.token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("加密 Bot Token 失败"))
		return
	}
	operationCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.stopTelegramRuntime(operationCtx); err != nil {
		writeError(w, http.StatusConflict, errors.New("停止旧 Bot 超时，请稍后重试"))
		return
	}
	if err := a.store.SaveTelegramConfig(operationCtx, cipher, challenge.botID, challenge.username, challenge.ownerID, true); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("保存 Bot 配置失败"))
		return
	}
	if err := a.startTelegramRuntime(a.telegramParent, challenge.token, challenge.ownerID); err != nil {
		_ = a.store.SetTelegramEnabled(context.Background(), false)
		writeError(w, http.StatusBadGateway, errors.New("Bot 启动失败"))
		return
	}
	writeJSON(w, http.StatusOK, telegramSettingsView{
		Configured: true, Enabled: true, Running: true, BotID: challenge.botID,
		BotUsername: challenge.username, OwnerTelegramID: challenge.ownerID,
	})
}

func (a *App) updateTelegramSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &request); err != nil || request.Enabled == nil {
		writeError(w, http.StatusBadRequest, errors.New("请求格式无效"))
		return
	}
	config, cipher, err := a.store.LoadTelegramConfig(r.Context())
	if errors.Is(err, store.ErrTelegramConfigMissing) {
		writeError(w, http.StatusConflict, errors.New("请先绑定 Telegram Bot"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("读取 Telegram 配置失败"))
		return
	}
	operationCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if !*request.Enabled {
		if err := a.store.SetTelegramEnabled(operationCtx, false); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("保存 Bot 开关失败"))
			return
		}
		if err := a.stopTelegramRuntime(operationCtx); err != nil {
			writeError(w, http.StatusConflict, errors.New("Bot 停止超时"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if a.telegramIsRunning() {
		if err := a.store.SetTelegramEnabled(operationCtx, true); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("保存 Bot 开关失败"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key, err := a.telegramEncryptionKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("读取 Bot 安全存储失败"))
		return
	}
	token, err := decryptTelegramToken(key, cipher)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("Bot Token 无法解密，请重新绑定"))
		return
	}
	if err := a.startTelegramRuntime(a.telegramParent, token, config.OwnerTelegramID); err != nil {
		writeError(w, http.StatusBadGateway, errors.New("Bot 启动失败"))
		return
	}
	if err := a.store.SetTelegramEnabled(operationCtx, true); err != nil {
		_ = a.stopTelegramRuntime(context.Background())
		writeError(w, http.StatusInternalServerError, errors.New("保存 Bot 开关失败"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) deleteTelegramSettings(w http.ResponseWriter, r *http.Request) {
	operationCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := a.stopTelegramRuntime(operationCtx); err != nil {
		writeError(w, http.StatusConflict, errors.New("Bot 停止超时"))
		return
	}
	if err := a.store.DeleteTelegramConfig(operationCtx); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("解除 Bot 绑定失败"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) telegramEncryptionKey() ([32]byte, error) {
	a.telegramMu.Lock()
	defer a.telegramMu.Unlock()
	if a.telegramKeyReady {
		return a.telegramKey, nil
	}
	key, err := loadOrCreateTelegramKey(a.config.DatabasePath)
	if err != nil {
		return [32]byte{}, err
	}
	a.telegramKey, a.telegramKeyReady = key, true
	return key, nil
}

func (a *App) telegramIsRunning() bool {
	a.telegramMu.Lock()
	defer a.telegramMu.Unlock()
	if a.telegram == nil {
		return false
	}
	select {
	case <-a.telegram.done:
		return false
	default:
		return true
	}
}

func telegramVerificationCode() (string, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", value.Int64()), nil
}

func telegramChallengeID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
