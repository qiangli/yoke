package cligw

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/gateway"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// The cligw server: the llmgw gateway wired to fleet-agent worker pools.
//
// llmgw owns the request spine (admission, jobs, slots, SSE, usage tail) and
// cligw owns the ROUTE. Those two facts are in tension, because the gateway
// has a resolver of its own: it would widen a model name to a class, pick a
// backend by warm/least-loaded, and fail over down its own ranking. cligw's
// Router already answers that question with quota headroom, the reserve
// floor, operator weights, breaker state and band escalation, and its answer
// is the auditable one (X-Bashy-Routed, usage.jsonl).
//
// So the two are joined this way, and the direction matters:
//
//  1. The routing middleware runs BEFORE the gateway. It authorizes, reads
//     the body once, parses the request model into a cligw Selector, and
//     calls Router.Route. The decision is stamped on the response and stored
//     in the REQUEST CONTEXT.
//  2. The Catalog adapter the gateway resolves through reads that context and
//     returns exactly ONE row: {Name: the model string the client sent,
//     Backend: the agent the Router chose}. With one candidate the gateway's
//     own selection tiers (affinity, loaded-model preference, least-loaded)
//     are all no-ops and its failover slate collapses to the primary — the
//     gateway calls the backend the Router decided and nothing else.
//  3. Config.Backend maps that agent name to its pool, creating the pool on
//     first use, so the fleet's agents are all candidates and only the ones
//     actually routed to (or prewarmed) ever cost a process.
//
// Re-selection after a failure is therefore the Router's job on the NEXT
// request, through the breaker this middleware trips — never a silent
// in-request substitution the audit header would not describe.

// Server-wide constants. The bind default is loopback and the port is bashy's
// own, never 11434 (ollama's) — a cligw on the ollama port would silently
// answer for a different engine.
const (
	// DefaultPort is the loopback port `llm serve` binds when none is given:
	// the host's model door (pkg/broker), 24556 = "AILLM" on a keypad.
	DefaultPort = 24556

	// DefaultRequestTimeout bounds one request end to end, queue wait
	// included. An agent CLI turn is slow, so this is generous; it exists
	// so a wedged worker cannot hold a client forever.
	DefaultRequestTimeout = 10 * time.Minute

	// RoutedHeader carries the auditable routing decision.
	RoutedHeader = "X-Bashy-Routed"
	// FilterHeader narrows the candidate agents for one request.
	FilterHeader = "X-Bashy-Filter"
	// PolicyHeader selects a named routing policy for one request.
	PolicyHeader = "X-Bashy-Policy"

	// Principal is the single accounting identity. cligw serves the owner's
	// own seats with the owner's own tools; there is no multi-tenancy here
	// and a second principal would be a different security model.
	Principal = "owner"

	// Schema versions of the JSON envelopes this package serves or writes.
	HealthSchemaVersion   = "bashy-cligw-health-v1"
	EndpointSchemaVersion = "bashy-cligw-endpoint-v1"
	UsageSchemaVersion    = "bashy-cligw-usage-v1"

	// maxRequestBody mirrors the gateway's own limit: the middleware reads
	// the body before the gateway does, so it must not be the looser one.
	maxRequestBody = 32 << 20
)

// Bind modes for Serve.
const (
	BindLoopback = "loopback"
	BindLAN      = "lan"
	// BindUnixPrefix introduces a unix-socket bind: "unix:/path/to/sock".
	BindUnixPrefix = "unix:"
)

// UsageRecorder is the JSONL sink OnUsage writes through. *JSONLRecorder
// implements it, so routing decisions and served-token usage land in one
// serialized writer on one file.
type UsageRecorder interface {
	Append(context.Context, any) error
}

// ServerOptions configures NewServer. The zero value is a working production
// server: the standard fleet rings, policy.yaml under the bashy home, the
// generated bearer token, and the prefork pool defaults.
type ServerOptions struct {
	// Catalog is the fleet inventory. Nil loads the standard merged rings.
	Catalog *FleetCatalog

	// Policy is the routing policy. Nil loads PolicyFile when set and
	// otherwise policy.yaml under the bashy home.
	Policy     *Policy
	PolicyFile string

	// Token is the bearer token. Empty reads (or creates) the token file.
	Token string

	// Pool is the per-agent prefork configuration. The zero value uses
	// DefaultPoolConfig; MaxWorkers is additionally capped by the policy's
	// per-vendor concurrency cap for the agent's provider.
	Pool PoolConfig

	// Autoscale configures the prewarmer. Ranker and Headroom are always
	// set by the server (they are the router's own view) and any value
	// here for them is ignored.
	Autoscale AutoscaleConfig

	// Quota is the llmbudget view used for routing and prewarming. Nil
	// uses llmbudget directly.
	Quota QuotaSource

	// Recorder persists routing decisions. Nil appends to usage.jsonl.
	Recorder Recorder

	// Usage persists served-token accounting. Nil appends to usage.jsonl.
	Usage UsageRecorder

	// Breaker is shared with the Router: a CLI failure trips it and the
	// next request routes elsewhere. Nil gets a private breaker.
	Breaker *sched.Breaker

	// RequestTimeout bounds one request; zero uses DefaultRequestTimeout
	// and a negative value disables the bound.
	RequestTimeout time.Duration

	// Logger receives routing refusal diagnostics. Nil uses slog.Default().
	Logger *slog.Logger

	// Prewarm pre-creates the pool of each band's routing leader at
	// startup instead of waiting for the first request to that band.
	Prewarm bool
}

// rankTTL is how long a per-band routing rank is reused for SPARE PLACEMENT.
// Quota headroom moves over minutes; the scaler asks every second.
const rankTTL = 5 * time.Second

// rankEntry is one cached band rank.
type rankEntry struct {
	agents []string
	at     time.Time
}

// Server is one cligw front door: pools, router, autoscaler and the gateway
// handler over them.
type Server struct {
	catalog  *FleetCatalog
	policy   Policy
	token    string
	quota    QuotaSource
	breaker  *sched.Breaker
	recorder Recorder
	usage    UsageRecorder
	poolCfg  PoolConfig
	timeout  time.Duration
	log      *slog.Logger

	router *Router
	scaler *Autoscaler
	// history is cligw's OWN prompt→band memory, deliberately NOT the
	// gateway's: the gateway records a resolution with tier 0 (it resolves
	// by model name, not by band), and a tier-0 hit would classify the next
	// identical `auto` prompt down to L1.
	history *sched.HistoryBuffer
	handler http.Handler

	ctx    context.Context
	cancel context.CancelFunc

	rankMu sync.Mutex
	ranked map[int]rankEntry

	mu       sync.Mutex
	pools    map[string]*Pool
	backends map[string]*AgentBackend
	order    []string
	closed   bool

	// stickyMu guards the reserved sticky sessions below, separately from mu:
	// DialSticky needs the pool (mu) and the session budget (stickyMu) without
	// nesting them the other way anywhere.
	stickyMu    sync.Mutex
	stickyCount map[string]int
	stickySet   map[*StickySession]struct{}
}

// NewServer wires the fleet catalog, the router, the autoscaler and the llmgw
// gateway into one handler. Nothing is spawned until a request routes to an
// agent (or Prewarm asks for a band leader's pool).
func NewServer(opts ServerOptions) (*Server, error) {
	catalog := opts.Catalog
	if catalog == nil {
		catalog = LoadFleetCatalog()
	}
	policy, err := serverPolicy(opts)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		if token, err = LoadOrCreateToken(); err != nil {
			return nil, err
		}
	}
	quota := opts.Quota
	if quota == nil {
		quota = llmBudgetQuota{}
	}
	quota = cacheHeadroom(quota)
	breaker := opts.Breaker
	if breaker == nil {
		breaker = sched.NewBreaker()
	}
	recorder := opts.Recorder
	usageSink := opts.Usage
	if recorder == nil || usageSink == nil {
		shared := &JSONLRecorder{Path: usagePath()}
		if recorder == nil {
			recorder = shared
		}
		if usageSink == nil {
			usageSink = shared
		}
	}
	poolCfg := opts.Pool
	if poolCfg == (PoolConfig{}) {
		poolCfg = DefaultPoolConfig()
	}
	timeout := opts.RequestTimeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		catalog: catalog, policy: policy, token: token, quota: quota,
		breaker: breaker, recorder: recorder, usage: usageSink,
		poolCfg: poolCfg, timeout: timeout, log: log,
		history: sched.NewHistoryBuffer(),
		ctx:     ctx, cancel: cancel,
		ranked:      map[int]rankEntry{},
		pools:       map[string]*Pool{},
		backends:    map[string]*AgentBackend{},
		stickyCount: map[string]int{},
		stickySet:   map[*StickySession]struct{}{},
	}
	s.router = NewRouter(catalog, policy,
		WithPoolState(s), WithQuotaSource(quota), WithBreaker(breaker), WithRecorder(recorder))

	scale := opts.Autoscale
	scale.Ranker = RankerFunc(s.Rank)
	scale.Headroom = s.agentHeadroom
	if scale.ReserveFloor <= 0 {
		scale.ReserveFloor = policy.ReserveFloor
	}
	if scale.VendorMaxWorkers == nil && len(policy.VendorConcurrencyCaps) > 0 {
		scale.VendorMaxWorkers = policy.VendorConcurrencyCaps
	}
	s.scaler = NewAutoscaler(scale)
	s.handler = s.buildHandler()
	if opts.Prewarm {
		s.Prewarm()
	}
	return s, nil
}

func serverPolicy(opts ServerOptions) (Policy, error) {
	if opts.Policy != nil {
		policy := *opts.Policy
		if err := policy.Validate(); err != nil {
			return Policy{}, err
		}
		return policy, nil
	}
	if strings.TrimSpace(opts.PolicyFile) != "" {
		return LoadPolicyFile(opts.PolicyFile)
	}
	return LoadPolicy()
}

// Handler returns the HTTP surface: the llmgw gateway behind cligw's routing
// middleware, plus cligw's own /v1/models and /health.
func (s *Server) Handler() http.Handler { return s.handler }

// ResolveAgent answers "which agent would serve this model right now" — a
// band, model, alias or agent selector resolved through the router exactly as
// a request would be. The broker freezes the answer in a sticky binding.
// `auto` has no fixed answer (it depends on the prompt) and is refused.
func (s *Server) ResolveAgent(ctx context.Context, model, filterHeader string) (Agent, error) {
	filter, err := ParseFilterWithDefault(filterHeader, s.policy.Filter)
	if err != nil {
		return Agent{}, err
	}
	if filter.Slash != "" {
		return Agent{}, fmt.Errorf("cligw: slash=%s runs a tool command and cannot be frozen into a sticky identity", filter.Slash)
	}
	sel, err := s.catalog.ParseModelSelector(model)
	if err != nil {
		return Agent{}, err
	}
	if sel.Kind == SelectorAuto {
		return Agent{}, errors.New("cligw: `auto` picks a band per prompt and cannot be frozen; name a band, model or agent")
	}
	decision, err := s.router.Route(ctx, sel, filter, "", "")
	if err != nil {
		return Agent{}, err
	}
	a, ok := s.catalog.Agent(decision.Agent)
	if !ok {
		return Agent{}, fmt.Errorf("cligw: routed to unknown agent %q", decision.Agent)
	}
	return a, nil
}

// Agent returns the fleet agent name as the catalog projects it right now
// (cached for a few seconds). The broker uses it to re-check a frozen
// identity's declared settings — effort — against the live binding.
func (s *Server) Agent(name string) (Agent, bool) { return s.catalog.Agent(name) }

// VendorModel is the provider-side model id the agent's tool is handed
// (the tool-specific id when the registry has one).
func (s *Server) VendorModel(a Agent) string {
	m, ok := s.catalog.fleet.Model(a.Model)
	if !ok {
		return ""
	}
	if id := m.ToolIDs[a.Tool]; id != "" {
		return id
	}
	return m.UpstreamID
}

// VersionProbeArgv is the argv that reports tool's version for an identity:
// the tool definition's version probe, run on the executable a worker launches
// (cli.binary), so a pinned CLI copy is the version the identity names. An
// unknown tool falls back to `<tool> --version`.
func (s *Server) VersionProbeArgv(tool string) []string {
	t, ok := s.catalog.Registry().Tool(tool)
	if !ok {
		return []string{tool, "--version"}
	}
	argv := t.VersionProbeArgv()
	if len(argv) == 0 {
		return []string{t.Binary(), "--version"}
	}
	argv[0] = t.Binary()
	return argv
}

// Router returns the live router, so a host can inspect decision history.
func (s *Server) Router() *Router { return s.router }

// Autoscaler returns the live prewarmer.
func (s *Server) Autoscaler() *Autoscaler { return s.scaler }

// Token returns the bearer token callers must present.
func (s *Server) Token() string { return s.token }

// Close cancels every pool and stops the control loop. Idle workers are
// killed with their process groups.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	pools := make([]*Pool, 0, len(s.pools))
	for _, pool := range s.pools {
		pools = append(pools, pool)
	}
	s.mu.Unlock()
	s.cancel()
	for _, pool := range pools {
		_ = pool.Close()
	}
	// Reserved sticky sessions retire with the server, like pools. Copy the
	// set: Close drops each reservation, which mutates it.
	s.stickyMu.Lock()
	sessions := make([]*StickySession, 0, len(s.stickySet))
	for sess := range s.stickySet {
		sessions = append(sessions, sess)
	}
	s.stickyMu.Unlock()
	for _, sess := range sessions {
		_ = sess.Close()
	}
	return nil
}

func (s *Server) buildHandler() http.Handler {
	gw := gateway.New(gateway.Config{
		Catalog:   routingCatalog{s},
		Backend:   s.lookupBackend,
		Backends:  s.gatewayBackends,
		Authorize: s.Authorize,
		Breaker:   s.breaker,
		OnUsage:   s.onUsage,
	})
	routed := s.route(gw)

	mux := http.NewServeMux()
	for _, prefix := range []string{"", gateway.AliasPrefix} {
		mux.Handle("POST "+prefix+gateway.ChatCompletionsPath, routed)
		mux.HandleFunc("GET "+prefix+gateway.ModelsPath, s.listModels)
		mux.HandleFunc("GET "+prefix+gateway.HealthPath, s.serveHealth)
		mux.HandleFunc("POST "+prefix+gateway.EmbeddingsPath, s.noEmbeddings)
	}
	mux.Handle("POST "+gateway.MessagesPath, routed)
	mux.Handle("POST "+gateway.AnthropicMessagesPath, routed)
	mux.Handle("/", gw)
	return mux
}

// Authorize implements the gateway's one required hook: the bearer token from
// the token file, compared in constant time. The Anthropic surface's x-api-key
// spelling is accepted too, because that is the header its clients send.
func (s *Server) Authorize(r *http.Request) (string, int, error) {
	presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if presented == "" {
		presented = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if presented == "" {
		return "", 0, errors.New("missing bearer token (see `bashy llm env`)")
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) != 1 {
		return "", 0, errors.New("invalid bearer token (see `bashy llm env`)")
	}
	return Principal, 0, nil
}

// routeState is the middleware's per-request decision, handed to the catalog
// adapter through the request context.
type routeState struct {
	Model    string
	Decision Decision
	Band     int
}

type routeStateKey struct{}

func routeStateFrom(ctx context.Context) *routeState {
	st, _ := ctx.Value(routeStateKey{}).(*routeState)
	return st
}

// route is the middleware that turns a request model into a Router decision
// before the gateway resolves anything.
func (s *Server) route(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := s.Authorize(r); err != nil {
			writeJSON(w, http.StatusUnauthorized, errorEnvelope(err.Error()))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
		_ = r.Body.Close()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorEnvelope("read body: "+err.Error()))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		// The gateway strips the @auto suffix before it looks a model up,
		// so the row name the catalog adapter returns must be stripped
		// too or the two would never meet.
		model := strings.TrimSuffix(peekModel(body), resolve.AutoSuffix)
		if model == "" {
			// No model to route. The gateway owns that error message.
			next.ServeHTTP(w, r)
			return
		}
		filter, err := ParseFilterWithDefault(r.Header.Get(FilterHeader), s.policy.Filter)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorEnvelope(err.Error()))
			return
		}
		sel, err := s.catalog.ParseModelSelector(model)
		if err != nil {
			var unknown *UnknownSelectorError
			status := http.StatusBadRequest
			if errors.As(err, &unknown) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, errorEnvelope(err.Error()))
			return
		}
		anthropicSurface := isAnthropicPath(r.URL.Path)
		if filter.Slash != "" {
			if err := s.catalog.CheckSlash(filter.Slash); err != nil {
				writeSlashError(w, anthropicSurface, slashFailure{Status: http.StatusBadRequest,
					Type: "tool_command_unknown", Message: err.Error(), Command: filter.Slash})
				return
			}
		}
		wasAuto := sel.Kind == SelectorAuto
		sel = sel.Classify(body, historyLookup{s.history})
		if filter.Slash != "" && !s.slashCandidatesExist(r.Context(), sel, s.policy.Filter.Merge(filter)) {
			writeSlashError(w, anthropicSurface, slashFailure{Status: http.StatusNotFound, Type: "tool_command_no_candidate",
				Message: fmt.Sprintf("cligw: no agent for model %q declares tool command %q (unmet requirement: slash=%s); tools declaring it: %s",
					model, filter.Slash, filter.Slash, strings.Join(s.catalog.toolsDeclaring(filter.Slash), ", ")),
				Command: filter.Slash})
			return
		}

		// A bad X-Bashy-Policy is the caller's mistake, not the host's, and
		// the Router reports it as a plain error the HTTP layer could only
		// turn into a 500.
		policyName := strings.TrimSpace(r.Header.Get(PolicyHeader))
		if policyName != "" && !validPolicyName(policyName) {
			writeJSON(w, http.StatusBadRequest, errorEnvelope(fmt.Sprintf(
				"cligw: unknown routing policy %q (want %s, %s, %s or prefer:<vendor>)",
				policyName, PolicyQuotaFirst, PolicyLatencyFirst, PolicyRoundRobin)))
			return
		}

		decision, err := s.router.Route(r.Context(), sel, filter, policyName, sched.DeriveSessionID(body))
		if err != nil {
			if r.Context().Err() != nil {
				// The client hung up mid-decision. Nothing to answer to.
				return
			}
			var routeErr *RouteError
			if errors.As(err, &routeErr) {
				if routeErr.Status == http.StatusServiceUnavailable || routeErr.Status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "5")
				}
				// The 429 body already names each refused candidate and its
				// exact reason; mirror it to the door log so a stuck door is
				// diagnosable without reproducing the request.
				s.log.Warn("cligw: route refused", "status", routeErr.Status, "reason", routeErr.Reason, "model", model)
				writeJSON(w, routeErr.Status, errorEnvelope(routeErr.Reason))
				return
			}
			writeJSON(w, http.StatusInternalServerError, errorEnvelope(err.Error()))
			return
		}
		if filter.Slash != "" {
			// A slash request runs the agent's vendor command; it takes no
			// completion worker, so no pool is created or consulted.
			w.Header().Set(RoutedHeader, decision.HeaderValue())
			s.serveSlash(w, r, body, model, decision, filter.Slash)
			return
		}
		if _, err := s.Backend(decision.Agent); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errorEnvelope(err.Error()))
			return
		}
		// A pool whose spawns all fail must not park the caller on the
		// queue until the request timeout: the pool retries on a widening
		// backoff, so without this the SECOND request waits as long as the
		// first. Trip the breaker instead and the next decision goes
		// somewhere that works.
		if err := s.checkSpawnable(decision.Agent); err != nil {
			s.breaker.Trip(decision.Agent, 0)
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, errorEnvelope(err.Error()))
			return
		}
		w.Header().Set(RoutedHeader, decision.HeaderValue())

		ctx := context.WithValue(r.Context(), routeStateKey{}, &routeState{
			Model: model, Decision: decision, Band: bandNumber(decision.Band),
		})
		if s.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, s.timeout)
			defer cancel()
		}
		if sel.Kind == SelectorBand {
			s.scaler.ObserveBand(sel.Band)
		}

		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r.WithContext(ctx))

		// The autoscaler learns λ and W from what actually ran, and the
		// breaker learns from what failed: a dead CLI must cost the agent
		// its place in the NEXT routing decision, which is the only way
		// re-selection happens here.
		s.scaler.Observe(decision.Agent, started, time.Since(started))
		if recorder.status >= http.StatusInternalServerError {
			s.breaker.Trip(decision.Agent, 0)
		}
		// Remember which band a classified prompt actually ran at, so a
		// similar prompt skips the heuristic next time. Only on success:
		// a band that failed is not a band worth repeating.
		if wasAuto && recorder.status > 0 && recorder.status < http.StatusBadRequest {
			s.history.Record(resolve.PromptPrefixKey(resolve.ExtractFirstUserPrompt(body)),
				decision.Agent, sel.Band, nil, true)
		}
	})
}

// statusRecorder remembers the status the gateway wrote. It forwards Flush so
// a streaming response is not buffered behind it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// routingCatalog is the resolve.Catalog the gateway resolves through. On a
// routed request it returns the Router's single answer; anywhere else it
// returns the plain fleet inventory.
type routingCatalog struct{ s *Server }

func (c routingCatalog) Rows(ctx context.Context, principal string) []resolve.ModelRow {
	st := routeStateFrom(ctx)
	if st == nil {
		return c.s.catalog.Rows(ctx, principal)
	}
	row := resolve.ModelRow{Name: st.Model, Backend: st.Decision.Agent, Class: st.Band}
	if agent, ok := c.s.agent(st.Decision.Agent); ok {
		row.Capabilities = cloneStrings(agent.Capabilities)
		row.Domains = cloneStrings(agent.Domains)
		row.ContextLen = agent.ContextLength
	}
	return []resolve.ModelRow{row}
}

func (c routingCatalog) Alias(ctx context.Context, name string) (int, []string, bool) {
	return c.s.catalog.Alias(ctx, name)
}

// historyLookup adapts the scheduler's history buffer to the resolver's
// PromptHistory seam, so `model: auto` classification improves with use.
type historyLookup struct{ buf *sched.HistoryBuffer }

func (h historyLookup) Lookup(key string) (resolve.PromptResolution, bool) {
	if h.buf == nil {
		return resolve.PromptResolution{}, false
	}
	rec, ok := h.buf.Lookup(key)
	if !ok {
		return resolve.PromptResolution{}, false
	}
	return resolve.PromptResolution{Tier: rec.Tier, Domains: rec.Domains, Success: rec.Success}, true
}

// Backend returns the agent's pool-backed gateway backend, creating the pool
// on first use. An agent that is not in the fleet inventory has no pool.
func (s *Server) Backend(agent string) (*AgentBackend, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("cligw: server is closed")
	}
	if backend, ok := s.backends[agent]; ok {
		s.mu.Unlock()
		return backend, nil
	}
	s.mu.Unlock()

	row, ok := s.agent(agent)
	if !ok {
		return nil, fmt.Errorf("cligw: agent %q is not a launchable fleet agent", agent)
	}

	s.mu.Lock()
	if backend, ok := s.backends[agent]; ok {
		s.mu.Unlock()
		return backend, nil
	}
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("cligw: server is closed")
	}
	pool := NewPool(s.ctx, agent, s.poolConfigFor(row))
	backend := NewAgentBackend(agent, row.Model, pool)
	s.pools[agent] = pool
	s.backends[agent] = backend
	s.order = append(s.order, agent)
	s.mu.Unlock()

	if err := s.scaler.Register(PoolRegistration{
		Agent: agent, Provider: row.Provider, Band: row.Band, Pool: pool,
	}); err != nil {
		return nil, err
	}
	return backend, nil
}

// DialSticky reserves one warm CLI for a sticky bind=worker/reset=none
// binding and returns its session. The session shares the agent's
// concurrency budget with the one-shot pool: pool workers plus reserved
// sessions never exceed the pool ceiling (already capped by the vendor seat),
// and a full room is refused fast instead of queueing behind the binding.
// Only stdin-stream-json tools can hold a session; anything else is
// ErrStickyUnsupported, anything full is ErrStickyCapped. The caller owns the
// session and must Close it; the count drops when the session closes.
func (s *Server) DialSticky(ctx context.Context, agent string) (*StickySession, error) {
	row, ok := s.agent(agent)
	if !ok {
		return nil, fmt.Errorf("cligw: agent %q is not a launchable fleet agent", agent)
	}
	if WarmMode(strings.TrimSpace(row.Warm)) != WarmStdinStreamJSON {
		warm := strings.TrimSpace(row.Warm)
		if warm == "" {
			warm = string(WarmCold)
		}
		return nil, fmt.Errorf("%w: agent %q runs tool %q with warm mode %q",
			ErrStickyUnsupported, agent, row.Tool, warm)
	}
	backend, err := s.Backend(agent)
	if err != nil {
		return nil, err
	}
	pool := backend.Pool
	max := pool.Config().MaxWorkers
	s.stickyMu.Lock()
	if pool.Total()+s.stickyCount[agent] >= max {
		s.stickyMu.Unlock()
		return nil, fmt.Errorf("%w: agent %q holds %d workers and sessions at ceiling %d",
			ErrStickyCapped, agent, pool.Total()+s.stickyCount[agent], max)
	}
	s.stickyCount[agent]++
	s.stickyMu.Unlock()
	sess, err := NewStickySession(ctx, agent)
	if err != nil {
		s.stickyMu.Lock()
		s.stickyCount[agent]--
		s.stickyMu.Unlock()
		return nil, err
	}
	s.stickyMu.Lock()
	s.stickySet[sess] = struct{}{}
	s.stickyMu.Unlock()
	sess.onClose = func() { s.releaseSticky(sess, agent) }
	return sess, nil
}

func (s *Server) releaseSticky(sess *StickySession, agent string) {
	s.stickyMu.Lock()
	defer s.stickyMu.Unlock()
	if _, ok := s.stickySet[sess]; !ok {
		return
	}
	delete(s.stickySet, sess)
	if s.stickyCount[agent] > 0 {
		s.stickyCount[agent]--
	}
}

// checkSpawnable reports a pool that has never produced a worker and has
// failed trying. It is deliberately not "any failure": a pool that served a
// request and then lost a spawn is degraded, not broken.
func (s *Server) checkSpawnable(agent string) error {
	pool := s.pool(agent)
	if pool == nil {
		return nil
	}
	stats := pool.Stats()
	if stats.Spawned == 0 && stats.Failed > 0 {
		return fmt.Errorf("cligw: agent %q has produced no worker in %d spawn attempts (see `bashy llm pools`)",
			agent, stats.Failed)
	}
	return nil
}

// poolConfigFor applies the vendor's concurrent-session cap from the policy:
// a pool may never hold more workers than the seat allows.
func (s *Server) poolConfigFor(agent Agent) PoolConfig {
	cfg := s.poolCfg
	if cap, ok := s.policy.VendorConcurrencyCaps[agent.Provider]; ok && cap > 0 && cap < cfg.MaxWorkers {
		cfg.MaxWorkers = cap
	}
	return cfg
}

func (s *Server) lookupBackend(name string) (gateway.Backend, bool) {
	backend, err := s.Backend(name)
	if err != nil {
		return nil, false
	}
	return backend, true
}

func (s *Server) gatewayBackends() []gateway.Backend {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]gateway.Backend, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.backends[name])
	}
	return out
}

// Prewarm creates the pool of every band's routing leader, so the first
// request to a band finds a warm worker instead of paying the spawn. It is
// the deliberate half of prewarming; the autoscaler does the rest from
// observed demand.
func (s *Server) Prewarm() []string {
	seen := map[string]struct{}{}
	var warmed []string
	for band := 1; band <= fleet.MaxBand; band++ {
		// The UNRESTRICTED rank: Prewarm is choosing which pool to create,
		// so it cannot be limited to the pools that already exist.
		ranked := s.router.Rank(s.ctx, band, Filter{}, "")
		if len(ranked) == 0 {
			continue
		}
		leader := ranked[0]
		if _, dup := seen[leader]; dup {
			continue
		}
		seen[leader] = struct{}{}
		if _, err := s.Backend(leader); err == nil {
			warmed = append(warmed, leader)
		}
	}
	return warmed
}

// Idle implements PoolState for the router: warm idle workers per agent.
func (s *Server) Idle(agent string) int {
	if pool := s.pool(agent); pool != nil {
		return pool.Stats().Idle
	}
	return 0
}

// Queued implements PoolState for the router: waiting acquisitions per agent.
func (s *Server) Queued(agent string) int {
	if pool := s.pool(agent); pool != nil {
		return pool.Stats().Queued
	}
	return 0
}

// Rank implements the autoscaler's Ranker: the band's agents in the order the
// router would pick them, so band spares sit on the next agent to be chosen.
//
// It ranks ONLY the agents this server holds a pool for, and caches the answer
// for rankTTL. Router.Rank uses cached headroom and never previews admission;
// restricting it still avoids rescoring a large fleet on every scaler tick.
func (s *Server) Rank(band int) []string {
	names := s.poolNames()
	if len(names) == 0 {
		return nil
	}
	s.rankMu.Lock()
	defer s.rankMu.Unlock()
	if entry, ok := s.ranked[band]; ok && time.Since(entry.at) < rankTTL {
		return entry.agents
	}
	ranked := s.router.Rank(s.ctx, band, Filter{}, "", names...)
	s.ranked[band] = rankEntry{agents: ranked, at: time.Now()}
	return ranked
}

// poolNames returns the agents with a live pool, in creation order.
func (s *Server) poolNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *Server) agentHeadroom(agent string) (float64, bool) {
	row, ok := s.agent(agent)
	if !ok {
		return 0, false
	}
	return s.quota.Headroom(s.ctx, row)
}

func (s *Server) pool(agent string) *Pool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pools[agent]
}

func (s *Server) agent(name string) (Agent, bool) { return s.catalog.Agent(name) }

// onUsage appends one served request's accounting to usage.jsonl, next to the
// routing decision the Router recorded for it.
func (s *Server) onUsage(principal, model, backend string, u openai.Usage, latency time.Duration) {
	if s.usage == nil {
		return
	}
	_ = s.usage.Append(context.Background(), UsageRecord{
		SchemaVersion: UsageSchemaVersion,
		At:            time.Now().UTC(),
		Principal:     principal,
		Model:         model,
		Agent:         backend,
		Usage:         u,
		LatencyMS:     latency.Milliseconds(),
	})
}

// UsageRecord is one served request's line in usage.jsonl.
type UsageRecord struct {
	SchemaVersion string       `json:"schema_version"`
	At            time.Time    `json:"at"`
	Principal     string       `json:"principal"`
	Model         string       `json:"model"`
	Agent         string       `json:"agent"`
	Usage         openai.Usage `json:"usage"`
	LatencyMS     int64        `json:"latency_ms"`
	// Command is the tool command a slash= request ran ("<tool>:<name>").
	Command string `json:"command,omitempty"`
}

// ModelListResponse is the OpenAI /v1/models envelope carrying cligw's band
// metadata on every entry.
type ModelListResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// listModels serves cligw's own /v1/models: the band aliases L1…L5 (and
// L1+…L5+), then every launchable registry model and agent. The gateway's
// generic listing cannot serve this — it has no band vocabulary, and the
// band aliases are not catalog rows.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.Authorize(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, errorEnvelope(err.Error()))
		return
	}
	filter, err := ParseFilterWithDefault(r.Header.Get(FilterHeader), s.policy.Filter)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorEnvelope(err.Error()))
		return
	}
	// The filter keys are accepted as query parameters too
	// (/v1/models?slash=plan); a query key overrides the header's.
	query, err := filterFromQuery(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorEnvelope(err.Error()))
		return
	}
	filter = filter.Merge(query)
	if err := s.catalog.CheckSlash(filter.Slash); err != nil {
		writeJSON(w, http.StatusBadRequest, errorEnvelope(err.Error()))
		return
	}
	entries := s.filterServable(s.catalog.ModelList(r.Context(), filter))
	labels := s.quotaLabels(r.Context())
	for i := range entries {
		entries[i].XQuota = labels(entries[i])
	}
	writeJSON(w, http.StatusOK, ModelListResponse{Object: "list", Data: entries})
}

// filterServable drops a model/agent row the fleet cannot currently serve —
// not installed, in breaker cooldown, or marked ineligible — so /v1/models
// never advertises a model that is guaranteed to 503. Band aliases (L1…L5
// and their +variants) are virtual tiers spanning many agents, not one
// candidate, so they are left as is.
func (s *Server) filterServable(entries []ModelEntry) []ModelEntry {
	out := make([]ModelEntry, 0, len(entries))
	for _, entry := range entries {
		if _, _, isBand := parseBand(entry.ID); isBand {
			out = append(out, entry)
			continue
		}
		if ok, _ := s.modelServable(entry.ID); ok {
			out = append(out, entry)
		}
	}
	return out
}

// modelServable reports whether id's row has at least one currently
// reachable agent, reusing Router.Servable — the exact rule behind the
// router's 503 "no matching candidate is installed and outside breaker
// cooldown" — as the one source of truth. An agent row maps to exactly one
// agent; a model row maps to every agent bound to that model, the same
// aggregation quotaLabels uses to project model-level metadata.
func (s *Server) modelServable(id string) (bool, string) {
	if agent, ok := s.agent(id); ok {
		return s.router.Servable(agent)
	}
	reason := "no agent is bound to this model"
	for _, candidate := range s.catalog.inventory() {
		if candidate.Model != id {
			continue
		}
		ok, candidateReason := s.router.Servable(candidate)
		if ok {
			return true, ""
		}
		reason = candidateReason
	}
	return false, reason
}

// quotaLabels returns the x_quota renderer for one listing. A band alias spans
// many seats and an untracked seat has no figure, so both are empty rather
// than a zero that reads as "exhausted".
//
// Headroom belongs to an agent's provider/account binding, not merely its
// model. An aggregate model row is projected through its first deterministic
// agent, while the per-agent memo lets that model row and agent row share one
// read without collapsing distinct accounts that happen to use one model.
func (s *Server) quotaLabels(ctx context.Context) func(ModelEntry) string {
	memo := map[string]string{}
	return func(entry ModelEntry) string {
		if _, _, isBand := parseBand(entry.ID); isBand {
			return ""
		}
		agent, ok := s.agent(entry.ID)
		if !ok {
			for _, candidate := range s.catalog.inventory() {
				if candidate.Model == entry.ID {
					agent, ok = candidate, true
					break
				}
			}
		}
		if !ok {
			return ""
		}
		if label, ok := memo[agent.Name]; ok {
			return label
		}
		label := ""
		if headroom, known := s.quota.Headroom(ctx, agent); known {
			label = fmt.Sprintf("%.2f", headroom)
		}
		memo[agent.Name] = label
		return label
	}
}

func (s *Server) noEmbeddings(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.Authorize(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, errorEnvelope(err.Error()))
		return
	}
	writeJSON(w, http.StatusNotFound, errorEnvelope(
		"cligw serves no embedding models: an agent CLI is a completion surface"))
}

// AgentHealth is one pool's entry in the health report.
type AgentHealth struct {
	Agent    string    `json:"agent"`
	Model    string    `json:"model"`
	Tool     string    `json:"tool"`
	Provider string    `json:"provider,omitempty"`
	Band     int       `json:"band,omitempty"`
	Warm     string    `json:"warm,omitempty"`
	Cooling  bool      `json:"cooling"`
	Stats    PoolStats `json:"stats"`
}

// PolicyHealth is the routing posture in force, so /health answers "why did
// it pick that" without reading the policy file.
type PolicyHealth struct {
	Default      string  `json:"default"`
	ReserveFloor float64 `json:"reserve_floor"`
	Escalate     string  `json:"escalate"`
	Filter       Filter  `json:"filter,omitempty"`
}

// HealthReport is the bashy-cligw-health-v1 envelope: breaker state per pool
// plus the autoscaler snapshot `llm pools` renders.
type HealthReport struct {
	SchemaVersion string            `json:"schema_version"`
	Status        string            `json:"status"`
	At            time.Time         `json:"at"`
	Agents        []AgentHealth     `json:"agents"`
	Autoscale     AutoscaleSnapshot `json:"autoscale"`
	Policy        PolicyHealth      `json:"policy"`
}

// Health builds the health report. Read-only: it never probes a pool and
// never spawns one, so polling it costs nothing.
func (s *Server) Health() HealthReport {
	s.mu.Lock()
	order := append([]string(nil), s.order...)
	backends := make(map[string]*AgentBackend, len(s.backends))
	pools := make(map[string]*Pool, len(s.pools))
	for name, backend := range s.backends {
		backends[name] = backend
		pools[name] = s.pools[name]
	}
	s.mu.Unlock()

	report := HealthReport{
		SchemaVersion: HealthSchemaVersion,
		Status:        "ok",
		At:            time.Now().UTC(),
		Agents:        make([]AgentHealth, 0, len(order)),
		Autoscale:     s.scaler.Snapshot(),
		Policy: PolicyHealth{
			Default: s.policy.Default, ReserveFloor: s.policy.ReserveFloor,
			Escalate: s.policy.Escalate, Filter: s.policy.Filter,
		},
	}
	available := 0
	for _, name := range order {
		entry := AgentHealth{Agent: name, Cooling: s.breaker.InCooldown(name)}
		if row, ok := s.agent(name); ok {
			entry.Model, entry.Tool, entry.Provider = row.Model, row.Tool, row.Provider
			entry.Band, entry.Warm = row.Band, row.Warm
		}
		if pool := pools[name]; pool != nil {
			entry.Stats = pool.Stats()
		}
		if !entry.Cooling {
			available++
		}
		report.Agents = append(report.Agents, entry)
	}
	if len(order) > 0 && available == 0 {
		report.Status = "degraded"
	}
	return report
}

func (s *Server) serveHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Health())
}

// Serve binds according to bind ("loopback", "lan" or "unix:PATH") and serves
// until ctx is done. It writes the endpoint file so `llm pools` and `llm env`
// can find a running server, and removes it on the way out.
func (s *Server) Serve(ctx context.Context, bind string, port int) error {
	listener, endpoint, err := Listen(bind, port)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, listener, endpoint)
}

// ServeListener serves an already-bound listener. `llm serve` binds first so
// it can print the base URL before it blocks, and binding twice would either
// race another process onto the port or (with --port 0) print an address
// nothing is listening on.
func (s *Server) ServeListener(ctx context.Context, listener net.Listener, endpoint Endpoint) error {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint.PID = os.Getpid()
	endpoint.StartedAt = time.Now().UTC()
	if err := WriteEndpoint(endpoint); err != nil {
		_ = listener.Close()
		return err
	}
	defer func() {
		_ = RemoveEndpoint()
		if endpoint.Socket != "" {
			_ = os.Remove(endpoint.Socket)
		}
	}()

	scalerCtx, stopScaler := context.WithCancel(ctx)
	defer stopScaler()
	go s.scaler.Run(scalerCtx)

	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 30 * time.Second}
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(listener) }()
	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return nil
	}
}

// Endpoint is the bashy-cligw-endpoint-v1 record of a running server: how to
// reach it, written 0600 under the cligw state directory.
type Endpoint struct {
	SchemaVersion string    `json:"schema_version"`
	BaseURL       string    `json:"base_url"`
	Socket        string    `json:"socket,omitempty"`
	Bind          string    `json:"bind"`
	Port          int       `json:"port,omitempty"`
	PID           int       `json:"pid,omitempty"`
	Anthropic     bool      `json:"anthropic"`
	StartedAt     time.Time `json:"started_at,omitempty"`
}

// OpenAIBaseURL is the value OPENAI_BASE_URL takes: the SDKs append
// "/chat/completions" to it, so it ends in /v1.
func (e Endpoint) OpenAIBaseURL() string { return strings.TrimSuffix(e.BaseURL, "/") + "/v1" }

// AnthropicBaseURL is the value ANTHROPIC_BASE_URL takes. Its clients append
// "/v1/messages", which is why the prefix stops at /anthropic.
func (e Endpoint) AnthropicBaseURL() string {
	if !e.Anthropic {
		return ""
	}
	return strings.TrimSuffix(e.BaseURL, "/") + "/anthropic"
}

// Listen binds the requested surface. loopback is the default and the only
// one that needs no argument; lan must be asked for explicitly, because a
// single-principal bearer token on a LAN port is an invitation. A unix socket
// is created 0600, which is the strongest of the three.
func Listen(bind string, port int) (net.Listener, Endpoint, error) {
	if port <= 0 {
		port = DefaultPort
	}
	return listen(bind, port)
}

// listen accepts port zero for isolated listeners in tests.
func listen(bind string, port int) (net.Listener, Endpoint, error) {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		bind = BindLoopback
	}
	endpoint := Endpoint{SchemaVersion: EndpointSchemaVersion, Bind: bind, Anthropic: true}

	if path, ok := strings.CutPrefix(bind, BindUnixPrefix); ok {
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, Endpoint{}, errors.New("cligw: unix bind needs a socket path (unix:/path/to/sock)")
		}
		if err := prepareSocketPath(path); err != nil {
			return nil, Endpoint{}, err
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			return nil, Endpoint{}, fmt.Errorf("cligw: listen on %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			_ = listener.Close()
			return nil, Endpoint{}, fmt.Errorf("cligw: restrict %s to the owner: %w", path, err)
		}
		endpoint.Bind, endpoint.Socket = BindUnixPrefix+path, path
		endpoint.BaseURL = "http://localhost"
		return listener, endpoint, nil
	}

	host := "127.0.0.1"
	switch bind {
	case BindLoopback:
	case BindLAN:
		host = ""
	default:
		return nil, Endpoint{}, fmt.Errorf("cligw: unknown bind %q (want loopback, lan or unix:PATH)", bind)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return nil, Endpoint{}, fmt.Errorf("cligw: listen on %s:%d: %w", host, port, err)
	}
	bound, _ := listener.Addr().(*net.TCPAddr)
	if bound != nil {
		port = bound.Port
	}
	endpoint.Port = port
	endpoint.BaseURL = fmt.Sprintf("http://%s", net.JoinHostPort(advertisedHost(bind), fmt.Sprint(port)))
	return listener, endpoint, nil
}

// prepareSocketPath removes a socket left behind by a crashed server, and
// refuses to touch one that still answers.
func prepareSocketPath(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cligw: create socket directory: %w", err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cligw: inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("cligw: %s exists and is not a socket", path)
	}
	if conn, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		return fmt.Errorf("cligw: %s is already being served", path)
	}
	return os.Remove(path)
}

// advertisedHost is the host a client should dial. For a LAN bind that is a
// real interface address, never the 0.0.0.0 the listener holds.
func advertisedHost(bind string) string {
	if bind != BindLAN {
		return "127.0.0.1"
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		return ipNet.IP.String()
	}
	return "127.0.0.1"
}

// StateDir is the cligw state directory: $BASHY_HOME/cligw, or
// ~/.bashy/cligw when the bashy home has not been relocated. Never
// os.UserConfigDir — bashy state lives under the bashy home.
func StateDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "cligw"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cligw: resolve bashy home: %w", err)
	}
	return filepath.Join(home, ".bashy", "cligw"), nil
}

// TokenPath is the bearer-token file.
func TokenPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "token"), nil
}

// EndpointPath is the running-server record.
func EndpointPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "endpoint.json"), nil
}

// LoadOrCreateToken reads the bearer token, generating a 256-bit one on first
// serve. The file is 0600 inside a 0700 directory: it is the only thing
// standing between a loopback port and the owner's seats.
func LoadOrCreateToken() (string, error) {
	path, err := TokenPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cligw: read token: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cligw: generate token: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("cligw: create state directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("cligw: write token: %w", err)
	}
	return token, nil
}

// WriteEndpoint records a running server, 0600.
func WriteEndpoint(endpoint Endpoint) error {
	path, err := EndpointPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cligw: create state directory: %w", err)
	}
	data, err := json.MarshalIndent(endpoint, "", "  ")
	if err != nil {
		return fmt.Errorf("cligw: encode endpoint: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("cligw: write endpoint: %w", err)
	}
	return nil
}

// ReadEndpoint returns the running server's record. A missing file is not an
// error: it yields the loopback default, which is what a client would have
// guessed anyway.
func ReadEndpoint() (Endpoint, error) {
	fallback := Endpoint{
		SchemaVersion: EndpointSchemaVersion, Bind: BindLoopback, Port: DefaultPort,
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", DefaultPort), Anthropic: true,
	}
	path, err := EndpointPath()
	if err != nil {
		return Endpoint{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fallback, nil
	}
	if err != nil {
		return Endpoint{}, fmt.Errorf("cligw: read endpoint: %w", err)
	}
	var endpoint Endpoint
	if err := json.Unmarshal(data, &endpoint); err != nil {
		return Endpoint{}, fmt.Errorf("cligw: decode endpoint: %w", err)
	}
	if strings.TrimSpace(endpoint.BaseURL) == "" {
		endpoint.BaseURL = fallback.BaseURL
	}
	return endpoint, nil
}

// RemoveEndpoint drops the running-server record.
func RemoveEndpoint() error {
	path, err := EndpointPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Client returns an HTTP client that reaches endpoint, dialing the unix
// socket when that is how the server was bound.
func (e Endpoint) Client() *http.Client {
	if e.Socket == "" {
		return &http.Client{Timeout: 10 * time.Second}
	}
	socket := e.Socket
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
}

// FetchHealth reads /health from a running server.
func FetchHealth(ctx context.Context, endpoint Endpoint, token string) (HealthReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(endpoint.BaseURL, "/")+gateway.HealthPath, nil)
	if err != nil {
		return HealthReport{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := endpoint.Client().Do(req)
	if err != nil {
		return HealthReport{}, fmt.Errorf("cligw: reach %s: %w (is `bashy llm serve` running?)", endpoint.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return HealthReport{}, fmt.Errorf("cligw: %s returned %s", endpoint.BaseURL+gateway.HealthPath, resp.Status)
	}
	var report HealthReport
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&report); err != nil {
		return HealthReport{}, fmt.Errorf("cligw: decode health: %w", err)
	}
	return report, nil
}

// peekModel reads just the model field, so the middleware does not decode a
// body that may carry megabytes of vision payload.
func peekModel(body []byte) string {
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Model)
}

func bandNumber(label string) int {
	band, _, _ := parseBand(label)
	return band
}

func errorEnvelope(message string) map[string]any {
	return map[string]any{"error": message}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
