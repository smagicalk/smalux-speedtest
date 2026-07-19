package store

import (
	"fmt"
	"testing"

	"smalux-speedtest/internal/model"
)

// TestListResultsLimitedAndCount 确认报告路径只扫描需要的行，同时通过
// 独立 COUNT 保留完整结果数。这避免大订阅任务为一张最多 72 行的图片
// 在 Go 内存中加载全部结果。
func TestListResultsLimitedAndCount(t *testing.T) {
	database, clients := newTransitionStore(t, 1)
	task := Task{ID: model.NewID(), Status: "completed", CandidateCount: 3, TopN: 1, Threads: 4, ProxyCount: 5, CreatedAt: now()}
	if err := database.CreateTask(t.Context(), task, []string{clients[0].ID}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		if err := database.SaveResult(t.Context(), model.SpeedResult{
			TaskID: task.ID, ClientID: clients[0].ID, ProxyID: fmt.Sprintf("proxy-%d", index),
			ProxyName: fmt.Sprintf("node-%d", index), Protocol: "socks", MaskedAddress: "*.example.com:1080",
			SpeedServerID: fmt.Sprintf("server-%d", index), LatencyMS: float64(index + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := database.CountResults(t.Context(), task.ID)
	if err != nil || count != 5 {
		t.Fatalf("CountResults = %d, %v", count, err)
	}
	limited, err := database.ListResultsLimited(t.Context(), task.ID, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("ListResultsLimited = %d rows, %v", len(limited), err)
	}
	if _, err := database.ListResultsLimited(t.Context(), task.ID, 0); err == nil {
		t.Fatal("invalid zero result limit was accepted")
	}
}
