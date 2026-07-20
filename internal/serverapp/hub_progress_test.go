package serverapp

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"

	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
)

// TestNormalizeProgressBindsTrustedFields confirms that a compromised Client
// cannot use transient SSE progress as a second channel for its Assignment.
func TestNormalizeProgressBindsTrustedFields(t *testing.T) {
	hub := NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client := store.Client{ID: "client"}
	connected := &peer{client: client}
	assignment := testAssignment("task")
	assignment.Proxies[0].Name = `vless://uuid:password@secret.example:443/path`
	task := &runtimeTask{
		assignment: assignment,
		targets:    map[string]string{client.ID: "running"},
		resultProxies: map[string]resultProxyIdentity{
			assignment.Proxies[0].ID: resultIdentityFromProxy(assignment.Proxies[0]),
		},
	}
	hub.peers[client.ID] = connected
	hub.tasks[assignment.TaskID] = task

	secret := `vless://uuid:password@192.0.2.10:443/private/path`
	progress, ok := hub.normalizeProgress(assignment.TaskID, connected, model.Progress{
		TaskID: assignment.TaskID, ProxyID: assignment.Proxies[0].ID,
		ProxyName: secret, Phase: progressPhaseDownload, Message: secret,
		Current: math.MaxInt, Total: math.MaxInt, RateBPS: math.Inf(1),
	})
	if !ok {
		t.Fatal("valid progress was rejected")
	}
	wantName := model.NormalizeProxyName(assignment.Proxies[0].Protocol, assignment.Proxies[0].Name, assignment.Proxies[0].Server)
	if progress.ProxyName != wantName || progress.Message != anonymousSpeedServerName || progress.Current != assignment.TopN || progress.Total != assignment.TopN || progress.RateBPS != 0 {
		t.Fatalf("unexpected normalized progress: %+v", progress)
	}
	if strings.Contains(fmt.Sprintf("%+v", progress), secret) {
		t.Fatalf("progress exposed assignment: %+v", progress)
	}

	invalid := model.Progress{TaskID: assignment.TaskID, ProxyID: assignment.Proxies[0].ID, Phase: "vless://secret"}
	if _, ok := hub.normalizeProgress(assignment.TaskID, connected, invalid); ok {
		t.Fatal("unknown progress phase was accepted")
	}
	invalid.Phase = progressPhaseLatency
	invalid.ProxyID = "unknown"
	if _, ok := hub.normalizeProgress(assignment.TaskID, connected, invalid); ok {
		t.Fatal("unknown progress proxy was accepted")
	}
}
