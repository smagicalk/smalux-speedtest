package telegrambot

import (
	"strings"
	"testing"
)

func TestProgressEditorAggregatesMultipleClients(t *testing.T) {
	task := Task{
		ID: "task-1", ClientCount: 2, ProxyCount: 2, TopN: 3,
		Clients: []ClientOption{{ID: "sh", Name: "Shanghai"}, {ID: "tyo", Name: "Tokyo"}},
	}
	editor := newProgressEditor(nil, 1, 1, task)
	editor.status, editor.results = "running", 6
	editor.clients["sh"] = &clientProgress{name: "Shanghai", status: "running", phase: "下载测速", current: 1, total: 3, rateBPS: 120_000_000}
	editor.clients["tyo"] = &clientProgress{name: "Tokyo", status: "completed", phase: "汇总结果"}
	text := editor.render()
	for _, want := range []string{"Client 1/2", "结果 6/12", "Shanghai", "下载测速 1/3", "120.00 Mbps", "Tokyo", "汇总结果"} {
		if !strings.Contains(text, want) {
			t.Fatalf("progress text missing %q:\n%s", want, text)
		}
	}
}
