package serverapp

import (
	"io"
	"log/slog"
	"testing"

	"smalux-speedtest/internal/store"
)

func TestReserveNextWorkEnforcesClientAndProxyLeases(t *testing.T) {
	hub := NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	first := store.Client{ID: "client-a"}
	second := store.Client{ID: "client-b"}
	assignment := schedulerTestAssignment("task")
	hub.AddTask(assignment, []string{first.ID, second.ID})
	t.Cleanup(func() {
		hub.mu.Lock()
		if task := hub.tasks[assignment.TaskID]; task != nil && task.expires != nil {
			task.expires.Stop()
		}
		hub.mu.Unlock()
	})
	hub.peers[first.ID] = &peer{client: first}
	hub.peers[second.ID] = &peer{client: second}

	firstWork := hub.reserveNextWork(nil)
	secondWork := hub.reserveNextWork(nil)
	if firstWork == nil || secondWork == nil {
		t.Fatal("two idle Clients did not receive work")
	}
	if firstWork.ref.clientID == secondWork.ref.clientID {
		t.Fatal("one Client received two concurrent work units")
	}
	if firstWork.ref.proxyID == secondWork.ref.proxyID {
		t.Fatal("two Clients received the same proxy concurrently")
	}
	if extra := hub.reserveNextWork(nil); extra != nil {
		t.Fatalf("scheduler exceeded Client capacity: %+v", extra.ref)
	}

	hub.mu.Lock()
	target := firstWork.task.work[firstWork.ref.clientID]
	firstWork.task.targets[firstWork.ref.clientID] = "running"
	target.completed[firstWork.ref.proxyID] = struct{}{}
	hub.releaseActiveWorkLocked(target)
	hub.mu.Unlock()
	next := hub.reserveNextWork(nil)
	if next == nil || next.ref.clientID != firstWork.ref.clientID {
		t.Fatal("freed Client did not receive another work unit")
	}
	if next.ref.proxyID == firstWork.ref.proxyID || next.ref.proxyID == secondWork.ref.proxyID {
		t.Fatalf("next proxy %q was completed or still leased", next.ref.proxyID)
	}
}

func TestProxyLeaseAppliesAcrossTasks(t *testing.T) {
	hub := NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	first := store.Client{ID: "client-a"}
	second := store.Client{ID: "client-b"}

	firstAssignment := testAssignment("task-a")
	secondAssignment := testAssignment("task-b")
	secondAssignment.Proxies[0].ID = "same-node-different-task-id"
	hub.AddTask(firstAssignment, []string{first.ID})
	hub.AddTask(secondAssignment, []string{second.ID})
	hub.peers[first.ID] = &peer{client: first}
	hub.peers[second.ID] = &peer{client: second}
	for _, taskID := range []string{firstAssignment.TaskID, secondAssignment.TaskID} {
		t.Cleanup(func() {
			hub.mu.Lock()
			if task := hub.tasks[taskID]; task != nil && task.expires != nil {
				task.expires.Stop()
			}
			hub.mu.Unlock()
		})
	}

	firstWork := hub.reserveNextWork(nil)
	if firstWork == nil {
		t.Fatal("first task did not reserve shared proxy")
	}
	if secondWork := hub.reserveNextWork(map[string]bool{firstWork.ref.clientID: true}); secondWork != nil {
		t.Fatalf("same proxy was leased across tasks: %+v", secondWork.ref)
	}
	if firstWork.task.proxyKeys[firstWork.ref.proxyID] != hub.tasks[secondAssignment.TaskID].proxyKeys[secondAssignment.Proxies[0].ID] {
		t.Fatal("identical outbound did not produce a stable in-memory fingerprint")
	}
}

func TestSchedulerCompletesFullMatrixWithoutConcurrentOverlap(t *testing.T) {
	hub := NewHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	clients := []store.Client{{ID: "client-a"}, {ID: "client-b"}, {ID: "client-c"}}
	assignment := schedulerTestAssignment("task-matrix")
	hub.AddTask(assignment, []string{clients[0].ID, clients[1].ID, clients[2].ID})
	t.Cleanup(func() {
		hub.mu.Lock()
		if task := hub.tasks[assignment.TaskID]; task != nil && task.expires != nil {
			task.expires.Stop()
		}
		hub.mu.Unlock()
	})
	for _, client := range clients {
		hub.peers[client.ID] = &peer{client: client}
	}

	completed := make(map[string]map[string]int, len(clients))
	for _, client := range clients {
		completed[client.ID] = make(map[string]int, len(assignment.Proxies))
	}
	total := len(clients) * len(assignment.Proxies)
	for count := 0; count < total; count++ {
		work := hub.reserveNextWork(nil)
		if work == nil {
			t.Fatalf("scheduler left capacity idle after %d/%d completed units", count, total)
		}
		hub.mu.Lock()
		if len(hub.clientWork) > len(clients) || len(hub.proxyWork) > len(assignment.Proxies) {
			hub.mu.Unlock()
			t.Fatal("scheduler exceeded Client or proxy capacity")
		}
		target := work.task.work[work.ref.clientID]
		work.task.targets[work.ref.clientID] = "running"
		completed[work.ref.clientID][work.ref.proxyID]++
		target.completed[work.ref.proxyID] = struct{}{}
		hub.releaseActiveWorkLocked(target)
		hub.mu.Unlock()
	}
	if work := hub.reserveNextWork(nil); work != nil {
		t.Fatalf("scheduler produced duplicate matrix work: %+v", work.ref)
	}
	for _, client := range clients {
		for _, proxy := range assignment.Proxies {
			if completed[client.ID][proxy.ID] != 1 {
				t.Fatalf("Client %q proxy %q completed %d times, want 1", client.ID, proxy.ID, completed[client.ID][proxy.ID])
			}
		}
	}
}
