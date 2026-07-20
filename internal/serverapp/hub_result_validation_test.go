package serverapp

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

// TestSaveResultBindsIdentityAndLimitsRows 模拟已认证但异常的 Client，确认结果只能引用
// Assignment 中的代理、代理展示身份由服务端覆盖、文本/数值有界，且唯一结果数不超过
// “代理数 × TopN”。同一合法键重发仍可幂等更新。
func TestSaveResultBindsIdentityAndLimitsRows(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "result-validation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	client, _, err := database.CreateClient(t.Context(), "result-client", nil)
	if err != nil {
		t.Fatal(err)
	}
	taskRecord := store.Task{
		ID: model.NewID(), Status: "queued", CandidateCount: 1, TopN: 1, Threads: 1,
		ProxyCount: 1, ClientCount: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := database.CreateTask(t.Context(), taskRecord, []string{client.ID}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(database, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assignment := testAssignment(taskRecord.ID)
	assignment.Proxies[0].Name = `vless://uuid:password@secret.example:443/path`
	assignment.Proxies[0].Server = "secret.example"
	assignment.Proxies[0].Outbound = []byte(`{"type":"socks","tag":"proxy","server":"secret.example","server_port":1080,"username":"private-user","password":"private-password"}`)
	hub.AddTask(assignment, []string{client.ID})
	if started, err := database.StartTarget(t.Context(), taskRecord.ID, client.ID); err != nil || !started {
		t.Fatalf("StartTarget = %v, %v", started, err)
	}
	connected := &peer{client: client}
	hub.mu.Lock()
	hub.peers[client.ID] = connected
	hub.tasks[taskRecord.ID].targets[client.ID] = "running"
	hub.mu.Unlock()
	defer func() {
		hub.mu.Lock()
		delete(hub.peers, client.ID)
		hub.mu.Unlock()
		if err := hub.CancelTask(t.Context(), taskRecord.ID); err != nil {
			t.Errorf("cancel task: %v", err)
		}
	}()

	base := model.SpeedResult{
		TaskID: taskRecord.ID, ClientID: client.ID, ProxyID: assignment.Proxies[0].ID,
		ProxyName: "spoofed", Protocol: "spoofed", MaskedAddress: "secret.example:443",
		SpeedServerID: "vless://uuid:password@secret.example:443", SpeedServerName: strings.Repeat("n", 400),
		SpeedServerHost: "192.0.2.10:443/private/path", Country: `{"password":"private-password"}`,
		Sponsor: "provider private-password", Error: "vless://uuid:password@secret.example:443",
		LatencyMS: -1, JitterMS: math.Inf(1), DownloadBPS: maxResultSpeedBPS * 2,
		UploadBPS: -100, DurationMS: math.MaxInt64, CreatedAt: "client-controlled-time",
	}
	hub.saveResult(t.Context(), connected, base)

	// 未知代理不能写入；同一代理的第二个测速节点又超过 TopN=1 的唯一键上限。
	unknown := base
	unknown.ProxyID = "unknown-proxy"
	unknown.SpeedServerID = "unknown-server"
	hub.saveResult(t.Context(), connected, unknown)
	extra := base
	extra.SpeedServerID = "server-2"
	hub.saveResult(t.Context(), connected, extra)
	// 原键重发不增加行数，并应更新测量值。
	duplicate := base
	duplicate.DownloadBPS = 321
	duplicate.SpeedServerName = strings.Repeat("n", 400)
	hub.saveResult(t.Context(), connected, duplicate)

	results, err := database.ListResults(t.Context(), taskRecord.ID)
	if err != nil || len(results) != 1 {
		t.Fatalf("results = %+v, %v", results, err)
	}
	result := results[0]
	proxy := assignment.Proxies[0]
	if value := safePublicResultLabel("vless://uuid:password@secret.example:443", maxResultNameRunes, proxy); value != "" {
		t.Fatalf("share URI accepted as public metadata: %q", value)
	}
	shortCredentialProxy := proxy
	shortCredentialProxy.Outbound = []byte(`{"type":"socks","tag":"proxy","server":"secret.example","server_port":1080,"username":"u","password":"p"}`)
	for _, value := range []string{"u", "p", "provider p", "region-u"} {
		if sanitized := safePublicResultLabel(value, maxResultSponsorRunes, shortCredentialProxy); sanitized != "" {
			t.Fatalf("short outbound credential accepted as public metadata: %q", sanitized)
		}
	}
	wantProxyName := model.NormalizeProxyName(proxy.Protocol, proxy.Name, proxy.Server)
	if result.ProxyName != wantProxyName || result.Protocol != proxy.Protocol || result.MaskedAddress != model.MaskAddress(proxy.Server, proxy.Port) {
		t.Fatalf("client overrode proxy identity: %+v", result)
	}
	if result.DownloadBPS != 321 || result.LatencyMS != 0 || result.JitterMS != 0 || result.UploadBPS != 0 {
		t.Fatalf("unexpected normalized metrics: %+v", result)
	}
	if result.DurationMS != int64(assignment.TimeoutSeconds)*1000 || result.CreatedAt == "client-controlled-time" {
		t.Fatalf("duration/time were not bounded: %+v", result)
	}
	if utf8.RuneCountInString(result.SpeedServerName) != maxResultNameRunes || result.Error != model.ResultErrorProxyTest || strings.Contains(result.Error, "secret.example") {
		t.Fatalf("result text was not safely normalized: name=%d error=%q", utf8.RuneCountInString(result.SpeedServerName), result.Error)
	}
	if !strings.HasPrefix(result.SpeedServerID, "server-") || strings.Contains(result.SpeedServerID, "vless") || result.SpeedServerHost != "" || result.Country != "" || result.Sponsor != "" {
		t.Fatalf("untrusted server metadata was persisted: %+v", result)
	}
	encoded := fmt.Sprintf("%+v", result)
	for _, secret := range []string{"vless://", "private-password", "secret.example", "192.0.2.10", "/private/path"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("persisted result contains %q: %s", secret, encoded)
		}
	}
}
