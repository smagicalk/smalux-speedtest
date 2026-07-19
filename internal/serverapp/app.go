package serverapp

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"smalux-speedtest/internal/importer"
	"smalux-speedtest/internal/model"
	"smalux-speedtest/internal/store"
	"smalux-speedtest/internal/subscription"
)

//go:embed web/*
var webFiles embed.FS

type Config struct {
	Listen        string
	DatabasePath  string
	AdminPassword string
	Logger        *slog.Logger
}

type App struct {
	config   Config
	store    *store.Store
	hub      *Hub
	fetcher  *subscription.Fetcher
	template *template.Template
	sessions *sessionStore
	server   *http.Server
}

type pageData struct {
	CSRF string
}

func New(ctx context.Context, config Config) (*App, error) {
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	database, err := store.Open(config.DatabasePath)
	if err != nil {
		return nil, err
	}
	if err := database.BootstrapAdmin(ctx, config.AdminPassword); err != nil {
		database.Close()
		return nil, err
	}
	templates, err := template.ParseFS(webFiles, "web/*.html")
	if err != nil {
		database.Close()
		return nil, err
	}
	app := &App{
		config: config, store: database, fetcher: subscription.NewFetcher(), template: templates,
		sessions: newSessionStore(),
	}
	app.hub = NewHub(database, config.Logger)
	app.server = &http.Server{
		Addr:              config.Listen,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return app, nil
}

func (a *App) ListenAndServe() error {
	a.config.Logger.Info("server listening", "address", a.config.Listen, "database", a.config.DatabasePath)
	return a.server.ListenAndServe()
}

func (a *App) Shutdown(ctx context.Context) error {
	err := a.server.Shutdown(ctx)
	storeErr := a.store.Close()
	if err != nil {
		return err
	}
	return storeErr
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /ws/client", a.hub.ServeWebSocket)

	mux.Handle("GET /", a.requireAdmin(http.HandlerFunc(a.dashboard)))
	mux.Handle("GET /tasks/{id}", a.requireAdmin(http.HandlerFunc(a.taskPage)))
	mux.Handle("POST /logout", a.requireAdmin(a.csrf(http.HandlerFunc(a.logout))))
	mux.Handle("GET /api/clients", a.requireAdmin(http.HandlerFunc(a.listClients)))
	mux.Handle("POST /api/clients", a.requireAdmin(a.csrf(http.HandlerFunc(a.createClient))))
	mux.Handle("DELETE /api/clients/{id}", a.requireAdmin(a.csrf(http.HandlerFunc(a.revokeClient))))
	mux.Handle("GET /api/tasks", a.requireAdmin(http.HandlerFunc(a.listTasks)))
	mux.Handle("POST /api/tasks", a.requireAdmin(a.csrf(http.HandlerFunc(a.createTask))))
	mux.Handle("GET /api/tasks/{id}", a.requireAdmin(http.HandlerFunc(a.getTask)))
	mux.Handle("POST /api/tasks/{id}/cancel", a.requireAdmin(a.csrf(http.HandlerFunc(a.cancelTask))))
	mux.Handle("GET /api/tasks/{id}/events", a.requireAdmin(http.HandlerFunc(a.taskEvents)))
	mux.Handle("GET /api/tasks/{id}/results.csv", a.requireAdmin(http.HandlerFunc(a.resultsCSV)))
	return a.logRequests(mux)
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.session(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	_ = a.template.ExecuteTemplate(w, "login.html", nil)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !a.store.VerifyAdmin(r.Context(), r.FormValue("password")) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = a.template.ExecuteTemplate(w, "login.html", map[string]string{"Error": "密码错误"})
		return
	}
	session := a.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name: "smalux_session", Value: session.token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: requestIsTLS(r), MaxAge: 12 * 60 * 60,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if session, ok := a.session(r); ok {
		a.sessions.delete(session.token)
	}
	http.SetCookie(w, &http.Cookie{Name: "smalux_session", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "dashboard.html", pageData{CSRF: session.csrf})
}

func (a *App) taskPage(w http.ResponseWriter, r *http.Request) {
	session, _ := a.session(r)
	_ = a.template.ExecuteTemplate(w, "task.html", struct {
		CSRF   string
		TaskID string
	}{CSRF: session.csrf, TaskID: r.PathValue("id")})
}

func (a *App) listClients(w http.ResponseWriter, r *http.Request) {
	clients, err := a.store.ListClients(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type clientView struct {
		store.Client
		Online bool `json:"online"`
	}
	response := make([]clientView, 0, len(clients))
	for _, client := range clients {
		response = append(response, clientView{Client: client, Online: a.hub.Online(client.ID)})
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) createClient(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	client, token, err := a.store.CreateClient(r.Context(), strings.TrimSpace(input.Name), input.Labels)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"client": client, "token": token})
}

func (a *App) revokeClient(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.store.RevokeClient(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	a.hub.RevokeClient(r.Context(), id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := a.store.ListTasks(r.Context(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (a *App) createTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Source          string   `json:"source"`
		SubscriptionURL string   `json:"subscription_url"`
		ClientIDs       []string `json:"client_ids"`
		CandidateCount  int      `json:"candidate_count"`
		TopN            int      `json:"top_n"`
		Threads         int      `json:"threads"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(input.ClientIDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("select at least one client"))
		return
	}
	if input.SubscriptionURL != "" {
		content, err := a.fetcher.Fetch(r.Context(), input.SubscriptionURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if input.Source != "" {
			input.Source += "\n"
		}
		input.Source += content
	}
	parsed := importer.Parse(input.Source)
	if len(parsed.Proxies) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "没有可测速的代理", "import_errors": parsed.Errors})
		return
	}
	if input.CandidateCount == 0 {
		input.CandidateCount = 10
	}
	if input.TopN == 0 {
		input.TopN = 3
	}
	if input.Threads == 0 {
		input.Threads = 4
	}
	if input.CandidateCount < 1 || input.CandidateCount > 50 || input.TopN < 1 || input.TopN > 3 || input.TopN > input.CandidateCount || input.Threads < 1 || input.Threads > 32 {
		writeError(w, http.StatusBadRequest, errors.New("candidate_count must be 1-50, top_n must be 1-3, and threads must be 1-32"))
		return
	}
	clients, err := a.store.ListClients(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	validClients := make(map[string]bool)
	for _, client := range clients {
		validClients[client.ID] = client.Enabled
	}
	uniqueIDs := make([]string, 0, len(input.ClientIDs))
	seen := make(map[string]bool)
	for _, id := range input.ClientIDs {
		if !validClients[id] {
			writeError(w, http.StatusBadRequest, fmt.Errorf("client %s does not exist or is revoked", id))
			return
		}
		if !seen[id] {
			seen[id] = true
			uniqueIDs = append(uniqueIDs, id)
		}
	}
	taskID := model.NewID()
	task := store.Task{
		ID: taskID, Status: "queued", CandidateCount: input.CandidateCount, TopN: input.TopN, Threads: input.Threads,
		ProxyCount: len(parsed.Proxies), ClientCount: len(uniqueIDs), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := a.store.CreateTask(r.Context(), task, uniqueIDs); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	assignment := model.Assignment{
		TaskID: taskID, Proxies: parsed.Proxies, CandidateCount: input.CandidateCount, TopN: input.TopN, Threads: input.Threads, TimeoutSeconds: 600,
	}
	a.hub.AddTask(assignment, uniqueIDs)
	writeJSON(w, http.StatusCreated, map[string]any{"task": task, "import_errors": parsed.Errors})
}

func (a *App) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := a.store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	results, err := a.store.ListResults(r.Context(), task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task, "results": results})
}

func (a *App) cancelTask(w http.ResponseWriter, r *http.Request) {
	if err := a.hub.CancelTask(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

func (a *App) taskEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	channel, unsubscribe := a.hub.Subscribe(r.PathValue("id"))
	defer unsubscribe()
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case event := <-channel:
			fmt.Fprintf(w, "data: %s\n\n", eventJSON(event))
			flusher.Flush()
		case <-keepAlive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *App) resultsCSV(w http.ResponseWriter, r *http.Request) {
	results, err := a.store.ListResults(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="speedtest-results.csv"`)
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"client_id", "proxy", "protocol", "address", "server_id", "server", "latency_ms", "jitter_ms", "download_mbps", "upload_mbps", "error", "created_at"})
	for _, result := range results {
		_ = writer.Write([]string{
			csvCell(result.ClientID), csvCell(result.ProxyName), result.Protocol, result.MaskedAddress, result.SpeedServerID,
			csvCell(result.SpeedServerName), formatFloat(result.LatencyMS), formatFloat(result.JitterMS),
			formatFloat(result.DownloadBPS / 1_000_000), formatFloat(result.UploadBPS / 1_000_000), csvCell(result.Error), result.CreatedAt,
		})
	}
	writer.Flush()
}

func (a *App) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.session(r); !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, errors.New("authentication required"))
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.session(r)
		provided := r.Header.Get("X-CSRF-Token")
		if provided == "" {
			_ = r.ParseForm()
			provided = r.FormValue("csrf")
		}
		if !ok || provided == "" || provided != session.csrf {
			writeError(w, http.StatusForbidden, errors.New("invalid CSRF token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		a.config.Logger.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

func (a *App) session(r *http.Request) (*session, bool) {
	cookie, err := r.Cookie("smalux_session")
	if err != nil {
		return nil, false
	}
	return a.sessions.get(cookie.Value)
}

type session struct {
	token   string
	csrf    string
	expires time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore { return &sessionStore{sessions: make(map[string]*session)} }

func (s *sessionStore) create() *session {
	value := &session{token: secureToken(), csrf: secureToken(), expires: time.Now().Add(12 * time.Hour)}
	s.mu.Lock()
	s.sessions[value.token] = value
	s.mu.Unlock()
	return value
}

func (s *sessionStore) get(token string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[token]
	if !ok || time.Now().After(value.expires) {
		delete(s.sessions, token)
		return nil, false
	}
	return value, true
}

func (s *sessionStore) delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func secureToken() string {
	value := make([]byte, 32)
	_, _ = rand.Read(value)
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 6<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func requestIsTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func csvCell(value string) string {
	if value != "" && strings.ContainsRune("=+-@", rune(value[0])) {
		return "'" + value
	}
	return value
}

func formatFloat(value float64) string { return strconv.FormatFloat(value, 'f', 2, 64) }
