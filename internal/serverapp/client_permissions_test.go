package serverapp

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"smalux-speedtest/internal/store"
)

func TestClientOwnershipLimitsOperatorMutations(t *testing.T) {
	application, err := New(t.Context(), Config{Listen: ":0", DatabasePath: filepath.Join(t.TempDir(), "clients.db"), AdminPassword: "initial-password"})
	if err != nil {
		t.Fatal(err)
	}
	defer application.store.Close()
	server := newTestHTTPServer(t, application)
	defer server.Close()

	ownerClient, ownerCSRF := loginAdminForTest(t, server.URL, "admin", "initial-password")
	created := doAdminJSON(t, ownerClient, ownerCSRF, http.MethodPost, server.URL+"/api/clients", map[string]any{"name": "owner-node"})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("owner client create status = %d", created.StatusCode)
	}
	var ownerPayload struct {
		Client store.Client `json:"client"`
	}
	if err := json.NewDecoder(created.Body).Decode(&ownerPayload); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	createdAdmin := doAdminJSON(t, ownerClient, ownerCSRF, http.MethodPost, server.URL+"/api/admin-users", map[string]any{"username": "operator", "password": "operator-password"})
	createdAdmin.Body.Close()
	if createdAdmin.StatusCode != http.StatusCreated {
		t.Fatalf("operator create status = %d", createdAdmin.StatusCode)
	}
	operatorClient, operatorCSRF := loginAdminForTest(t, server.URL, "operator", "operator-password")
	clientsResponse, err := operatorClient.Get(server.URL + "/api/clients")
	if err != nil || clientsResponse.StatusCode != http.StatusOK {
		t.Fatalf("operator client list = %v, %v", clientsResponse, err)
	}
	var clients []struct {
		ID         string `json:"id"`
		Manageable bool   `json:"manageable"`
	}
	if err := json.NewDecoder(clientsResponse.Body).Decode(&clients); err != nil {
		t.Fatal(err)
	}
	clientsResponse.Body.Close()
	if len(clients) != 1 || clients[0].ID != ownerPayload.Client.ID || clients[0].Manageable {
		t.Fatalf("operator view of owner Client = %+v", clients)
	}
	response := doAdminJSON(t, operatorClient, operatorCSRF, http.MethodPatch, server.URL+"/api/clients/"+ownerPayload.Client.ID, map[string]any{"name": "hijack"})
	if response.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("operator owner edit status = %d: %s", response.StatusCode, body)
	}
	response.Body.Close()
	response = doAdminJSON(t, operatorClient, operatorCSRF, http.MethodPost, server.URL+"/api/clients", map[string]any{"name": "operator-node"})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("operator own client create status = %d", response.StatusCode)
	}
	var ownPayload struct {
		Client store.Client `json:"client"`
	}
	if err := json.NewDecoder(response.Body).Decode(&ownPayload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response = doAdminJSON(t, operatorClient, operatorCSRF, http.MethodPatch, server.URL+"/api/clients/"+ownPayload.Client.ID, map[string]any{"name": "operator-renamed", "labels": map[string]string{"region": "hk"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("operator own edit status = %d", response.StatusCode)
	}
	response.Body.Close()
	response = doAdminJSON(t, operatorClient, operatorCSRF, http.MethodPost, server.URL+"/api/tasks", map[string]any{"source": "ss://invalid", "client_ids": []string{ownerPayload.Client.ID}})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("operator owner task status = %d", response.StatusCode)
	}
	response.Body.Close()

	createTask := func(client *http.Client, csrf, clientID string) store.Task {
		t.Helper()
		createdTask := doAdminJSON(t, client, csrf, http.MethodPost, server.URL+"/api/tasks", map[string]any{
			"source": "socks5://example.com:1080#permission-test", "client_ids": []string{clientID},
		})
		if createdTask.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(createdTask.Body)
			createdTask.Body.Close()
			t.Fatalf("task create status = %d: %s", createdTask.StatusCode, body)
		}
		var payload struct {
			Task store.Task `json:"task"`
		}
		if err := json.NewDecoder(createdTask.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		createdTask.Body.Close()
		return payload.Task
	}
	ownerTask := createTask(ownerClient, ownerCSRF, ownerPayload.Client.ID)
	operatorTask := createTask(operatorClient, operatorCSRF, ownPayload.Client.ID)

	listResponse := doAdminJSON(t, operatorClient, operatorCSRF, http.MethodGet, server.URL+"/api/tasks", nil)
	var operatorTasks []store.Task
	if err := json.NewDecoder(listResponse.Body).Decode(&operatorTasks); err != nil {
		t.Fatal(err)
	}
	listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusOK || len(operatorTasks) != 1 || operatorTasks[0].ID != operatorTask.ID {
		t.Fatalf("operator task list = status:%d tasks:%+v", listResponse.StatusCode, operatorTasks)
	}

	for _, path := range []string{
		"/tasks/" + ownerTask.ID,
		"/api/tasks/" + ownerTask.ID,
		"/api/tasks/" + ownerTask.ID + "/events",
		"/api/tasks/" + ownerTask.ID + "/results.csv",
	} {
		forbidden, err := operatorClient.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		forbidden.Body.Close()
		if forbidden.StatusCode != http.StatusNotFound {
			t.Fatalf("operator access %s status = %d", path, forbidden.StatusCode)
		}
	}
	forbiddenCancel := doAdminJSON(t, operatorClient, operatorCSRF, http.MethodPost, server.URL+"/api/tasks/"+ownerTask.ID+"/cancel", nil)
	forbiddenCancel.Body.Close()
	if forbiddenCancel.StatusCode != http.StatusNotFound {
		t.Fatalf("operator cancel owner task status = %d", forbiddenCancel.StatusCode)
	}

	detail := doAdminJSON(t, operatorClient, operatorCSRF, http.MethodGet, server.URL+"/api/tasks/"+operatorTask.ID, nil)
	var detailPayload struct {
		Targets []store.TaskTarget `json:"targets"`
	}
	if err := json.NewDecoder(detail.Body).Decode(&detailPayload); err != nil {
		t.Fatal(err)
	}
	detail.Body.Close()
	if detail.StatusCode != http.StatusOK || len(detailPayload.Targets) != 1 || detailPayload.Targets[0].ClientID != ownPayload.Client.ID {
		t.Fatalf("operator own task detail = status:%d payload:%+v", detail.StatusCode, detailPayload)
	}

	ownerList := doAdminJSON(t, ownerClient, ownerCSRF, http.MethodGet, server.URL+"/api/tasks", nil)
	var allTasks []store.Task
	if err := json.NewDecoder(ownerList.Body).Decode(&allTasks); err != nil {
		t.Fatal(err)
	}
	ownerList.Body.Close()
	if ownerList.StatusCode != http.StatusOK || len(allTasks) != 2 {
		t.Fatalf("owner task list = status:%d tasks:%+v", ownerList.StatusCode, allTasks)
	}
	for _, task := range []store.Task{ownerTask, operatorTask} {
		canceled := doAdminJSON(t, ownerClient, ownerCSRF, http.MethodPost, server.URL+"/api/tasks/"+task.ID+"/cancel", nil)
		canceled.Body.Close()
		if canceled.StatusCode != http.StatusOK {
			t.Fatalf("owner cancel task %s status = %d", task.ID, canceled.StatusCode)
		}
	}
}

func newTestHTTPServer(t *testing.T, application *App) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local sockets are unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(application.routes())
	server.Listener = listener
	server.Start()
	return server
}
