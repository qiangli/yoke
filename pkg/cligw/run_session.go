package cligw

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/gateway"
)

const sessionPath = "/v1/cligw-sessions"

type runSessionOptions struct {
	Label       string        `json:"label"`
	Models      []string      `json:"models,omitempty"`
	Band        string        `json:"band,omitempty"`
	TTL         time.Duration `json:"ttl"`
	MaxRequests int           `json:"max_requests"`
}

type runSessionInfo struct {
	Label     string    `json:"label"`
	Token     string    `json:"token,omitempty"`
	Socket    string    `json:"socket"`
	ExpiresAt time.Time `json:"expires_at"`
	Models    []string  `json:"models"`
}

type runSessionScope struct {
	band   int
	models []string
}

func (scope runSessionScope) allows(a Agent) bool {
	if scope.band != 0 {
		return a.Band == scope.band
	}
	return slices.Contains(scope.models, a.Name)
}

type runSessionKey struct{}

func runSessionFrom(ctx context.Context) *runSession {
	session, _ := ctx.Value(runSessionKey{}).(*runSession)
	return session
}

// The broker's shell sessions describe lineage, its peer credentials identify
// persistent peers, and its door grants convey remote access. A run credential
// belongs to one listener and carries its own lifetime and request budget.
type runSession struct {
	info      runSessionInfo
	scope     runSessionScope
	hash      [sha256.Size]byte
	principal string
	server    *http.Server
	listener  net.Listener
	dir       string
	limit     int

	mu        sync.Mutex
	remaining int
	inFlight  int
	closed    bool
}

type runSessions struct {
	mu     sync.Mutex
	active map[string]*runSession
	closed bool
}

func (s *Server) sessionScope(ctx context.Context, opts runSessionOptions) (runSessionScope, error) {
	var scope runSessionScope
	if (len(opts.Models) == 0) == (opts.Band == "") {
		return scope, fmt.Errorf("give either --model BINDING[,BINDING] or --band L3|L4|L5")
	}
	if opts.Band != "" {
		switch opts.Band {
		case "L3", "L4", "L5":
			scope.band = bandNumber(opts.Band)
		default:
			return scope, fmt.Errorf("invalid --band %q (want L3, L4 or L5)", opts.Band)
		}
		selector := Selector{Kind: SelectorBand, Band: scope.band}
		for _, agent := range s.catalog.Candidates(ctx, selector, s.policy.Filter) {
			if ok, _ := s.router.Servable(agent); ok {
				scope.models = append(scope.models, agent.Name)
			}
		}
		if len(scope.models) == 0 {
			return scope, fmt.Errorf("no backend serves band %s; run `bashy llm pools` to check the available bindings", opts.Band)
		}
		return scope, nil
	}
	for _, name := range opts.Models {
		name = strings.TrimSpace(name)
		// A binding is looked up by its own name first: a binding may share
		// its name with a model (muse-wonder is both), and the selector
		// grammar prefers the model.
		agent, ok := s.agent(name)
		if !ok {
			sel, err := s.catalog.ParseModelSelector(name)
			if err != nil || sel.Kind != SelectorAgent {
				return scope, fmt.Errorf("%q is not a served binding; run `bashy llm pools` to check the available bindings", name)
			}
			agent, ok = s.agent(sel.Name)
		}
		if !ok || !s.policy.Filter.Match(agent) {
			return scope, fmt.Errorf("no backend serves binding %q; run `bashy llm pools` to check the available bindings", name)
		}
		if ok, reason := s.router.Servable(agent); !ok {
			return scope, fmt.Errorf("no backend serves binding %q (%s); run `bashy llm pools` to check the available bindings", name, reason)
		}
		scope.models = append(scope.models, agent.Name)
	}
	scope.models = uniqueStrings(scope.models)
	sort.Strings(scope.models)
	return scope, nil
}

func (s *Server) openRunSession(ctx context.Context, opts runSessionOptions) (runSessionInfo, int, error) {
	if runtime.GOOS == "windows" {
		return runSessionInfo{}, http.StatusServiceUnavailable, fmt.Errorf("owner-only run session sockets are unavailable on Windows")
	}
	label := strings.TrimSpace(opts.Label)
	if label == "" || opts.TTL <= 0 || opts.MaxRequests <= 0 {
		return runSessionInfo{}, http.StatusBadRequest, fmt.Errorf("--label is required and --ttl and --max-requests must be positive")
	}
	scope, err := s.sessionScope(ctx, opts)
	if err != nil {
		return runSessionInfo{}, http.StatusBadRequest, err
	}
	store := &s.runSessions
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return runSessionInfo{}, http.StatusServiceUnavailable, fmt.Errorf("model door is shutting down")
	}
	if store.active[label] != nil {
		return runSessionInfo{}, http.StatusConflict, fmt.Errorf("session label %q is already open; choose another label or run `bashy llm session close %s`", label, label)
	}
	token, err := generateToken()
	if err != nil {
		return runSessionInfo{}, http.StatusInternalServerError, err
	}
	// Use a short, private directory to fit Unix socket path limits, even
	// when the model door's state is inside a deeply nested workspace.
	temp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return runSessionInfo{}, http.StatusInternalServerError, err
	}
	dir, err := os.MkdirTemp(temp, "cligw-")
	if err != nil {
		return runSessionInfo{}, http.StatusInternalServerError, fmt.Errorf("create session socket directory: %w", err)
	}
	listener, endpoint, err := Listen(BindUnixPrefix+filepath.Join(dir, "run.sock"), 0)
	if err != nil {
		_ = os.Remove(dir)
		return runSessionInfo{}, http.StatusServiceUnavailable, err
	}

	hash := sha256.Sum256([]byte(token))
	principal := fmt.Sprintf("session:%s:%x", label, hash[:16])
	info := runSessionInfo{Label: label, Socket: endpoint.Socket, ExpiresAt: time.Now().UTC().Add(opts.TTL), Models: scope.models}
	run := &runSession{
		info: info, scope: scope, hash: hash, principal: principal, dir: dir,
		listener: listener, remaining: opts.MaxRequests, limit: s.admissionLimit(principal),
	}
	run.server = &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: run.handler(s.handler)}
	if store.active == nil {
		store.active = make(map[string]*runSession)
	}
	store.active[label] = run
	go run.server.Serve(listener)
	info.Token = token
	return info, http.StatusOK, nil
}

func (store *runSessions) close(label string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if run := store.active[label]; run != nil {
		run.close()
		delete(store.active, label)
	}
}

func (store *runSessions) closeAll() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.closed = true
	for _, run := range store.active {
		run.close()
	}
	clear(store.active)
}

func (run *runSession) close() {
	run.mu.Lock()
	run.closed = true
	run.mu.Unlock()
	_ = run.server.Close()
	// Close may race the first Serve, before http.Server tracks the listener.
	_ = run.listener.Close()
	_ = os.Remove(run.dir)
}

func (store *runSessions) list() []runSessionInfo {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]runSessionInfo, 0, len(store.active))
	for _, run := range store.active {
		out = append(out, run.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func (s *Server) serveRunSessions(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.Authorize(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, errorEnvelope(err.Error()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodPost:
		var opts runSessionOptions
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
			writeJSON(w, http.StatusBadRequest, errorEnvelope("invalid session options"))
			return
		}
		info, status, err := s.openRunSession(r.Context(), opts)
		if err != nil {
			writeJSON(w, status, errorEnvelope(err.Error()))
			return
		}
		writeJSON(w, status, info)
	case http.MethodDelete:
		label := strings.TrimSpace(r.URL.Query().Get("label"))
		if label == "" {
			writeJSON(w, http.StatusBadRequest, errorEnvelope("session label is required"))
			return
		}
		s.runSessions.close(label)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.runSessions.list())
	}
}

func (run *runSession) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := sha256.Sum256([]byte(requestToken(r)))
		if subtle.ConstantTimeCompare(hash[:], run.hash[:]) != 1 {
			writeRunError(w, r, http.StatusUnauthorized, "invalid session token")
			return
		}
		if !sessionRoute(r.Method, r.URL.Path) {
			writeRunError(w, r, http.StatusNotFound, "route is unavailable on the session socket")
			return
		}
		if status, message := run.admit(); status != 0 {
			writeRunError(w, r, status, message)
			return
		}
		defer func() {
			run.mu.Lock()
			run.inFlight--
			run.mu.Unlock()
		}()

		ctx, cancel := context.WithDeadline(r.Context(), run.info.ExpiresAt)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, runSessionKey{}, run)))
	})
}

func (run *runSession) admit() (int, string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	switch {
	case run.closed:
		return http.StatusUnauthorized, "session token was revoked"
	case !time.Now().Before(run.info.ExpiresAt):
		return http.StatusUnauthorized, "session token expired; open a new session"
	case run.remaining == 0:
		return http.StatusTooManyRequests, "session request cap reached (--max-requests)"
	case run.inFlight >= run.limit:
		return http.StatusTooManyRequests, "session principal in-flight cap reached"
	}
	run.remaining--
	run.inFlight++
	return 0, ""
}

func sessionRoute(method, path string) bool {
	switch path {
	case gateway.MessagesPath, gateway.AnthropicMessagesPath,
		gateway.ChatCompletionsPath, gateway.AliasPrefix + gateway.ChatCompletionsPath,
		"/v1/responses", gateway.AliasPrefix + "/v1/responses",
		gateway.EmbeddingsPath, gateway.AliasPrefix + gateway.EmbeddingsPath:
		return method == http.MethodPost
	case gateway.ModelsPath, gateway.AliasPrefix + gateway.ModelsPath:
		return method == http.MethodGet || method == http.MethodHead
	}
	return false
}

func writeRunError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if isAnthropicPath(r.URL.Path) {
		kind := "invalid_request_error"
		switch status {
		case http.StatusUnauthorized:
			kind = "authentication_error"
		case http.StatusForbidden:
			kind = "permission_error"
		case http.StatusTooManyRequests:
			kind = "rate_limit_error"
		}
		writeJSON(w, status, map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
		return
	}
	writeJSON(w, status, errorEnvelope(message))
}

func (s *Server) sessionAllowsSelector(ctx context.Context, run *runSession, sel Selector) bool {
	switch sel.Kind {
	case SelectorBand:
		return !sel.MinBand && sel.Band == run.scope.band
	case SelectorAgent, SelectorModel:
		return slices.ContainsFunc(s.catalog.Candidates(ctx, sel, Filter{}), run.scope.allows)
	}
	return false
}
