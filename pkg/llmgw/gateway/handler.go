package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
	"github.com/qiangli/yoke/pkg/llmgw/resolve"
	"github.com/qiangli/yoke/pkg/llmgw/sched"
)

// The gateway front door: one http.Handler serving the OpenAI-compatible
// inference surface over a pool of backends.
//
// Routes (each also reachable under the /openai alias prefix, so a client
// configured with a base URL of either ".../v1" or ".../openai/v1" works
// unchanged):
//
//	POST /v1/chat/completions
//	POST /v1/messages
//	POST /anthropic/v1/messages
//	POST /v1/embeddings
//	GET  /v1/models
//	GET  /health
//
// Everything host-specific arrives through Config: the model inventory is a
// resolve.Catalog, a backend is looked up by name, and every policy the
// gateway itself has no opinion about (who the caller is, which candidates
// their quota still permits, what to do when the pool cannot serve a model)
// is a hook. The gateway owns only the request spine and the scheduler
// components.

// Request-path constants. The /v1 paths are the wire contract; AliasPrefix
// is the second mount point for the same routes.
const (
	ChatCompletionsPath = "/v1/chat/completions"
	EmbeddingsPath      = "/v1/embeddings"
	ModelsPath          = "/v1/models"
	HealthPath          = "/health"

	// AliasPrefix mounts every /v1/* route a second time under
	// /openai/v1/*, the other spelling OpenAI-compatible clients use.
	AliasPrefix = "/openai"
)

// DefaultMaxInFlight is the per-principal admission cap applied when Config
// carries no AdmissionLimit hook.
const DefaultMaxInFlight = 4

// maxRequestBody bounds the buffered request body. A prompt plus vision
// payload is comfortably under this; anything larger is a caller trying to
// exhaust memory before routing has even started.
const maxRequestBody = 32 << 20

// AuthorizeFunc identifies the caller. principal is the scheduling and
// accounting identity (fairness, affinity, quota all key off it);
// priorityCeiling is the best service priority this caller may ask for
// (see EffectiveRequestPriority — lower is better). A non-nil error is a
// 401; so is an empty principal.
type AuthorizeFunc func(r *http.Request) (principal string, priorityCeiling int, err error)

// AllowModelFunc reports whether principal may address model. Optional; a
// nil hook permits every model. Applied to the inference spine AND to the
// /v1/models listing, so a caller never sees a model they cannot call.
type AllowModelFunc func(principal, model string) bool

// AdmissionLimitFunc returns the per-principal in-flight cap. Optional;
// a nil hook (or a non-positive result) uses DefaultMaxInFlight.
type AdmissionLimitFunc func(principal string) int

// RouteDefaultFunc returns the principal's configured routing posture (one
// of the resolve.Route* values). Optional; an empty or unknown value falls
// through to resolve.DefaultRoutePolicy. The per-request header and body
// overrides are handled by the gateway and always win.
type RouteDefaultFunc func(principal string) string

// FilterCandidatesFunc narrows the backends eligible to serve model for
// this principal — the seam for per-owner quota, sharing budgets, or any
// other host policy the gateway has no vocabulary for. Optional; a nil hook
// keeps every backend.
//
// Returning an EMPTY slice for a non-empty input is meaningful: it says
// "backends exist but policy excluded all of them", and the gateway answers
// 429 rather than 404, because a retry later is the honest advice.
type FilterCandidatesFunc func(ctx context.Context, principal, model string, backends []string) []string

// FilterCandidatesStatusFunc narrows the eligible backends and describes why
// none remain. When kept is non-empty, status and reason are ignored. When it
// is empty, status may be 404, 429, or 503; zero or any other value defaults
// to 429. This hook takes precedence over FilterCandidates when both are set.
//
// The status decides whether Fallback still gets a turn. 404 means "no
// backend here can hold this model", which is the same story as an empty
// pool, so the gateway escalates to Fallback exactly as it would for a
// model the catalog never listed. 429 and 503 mean the backends exist and
// a retry will likely work, so Fallback is NOT called and the client gets
// the status with this hook's reason.
type FilterCandidatesStatusFunc func(ctx context.Context, principal, model string, backends []string) (kept []string, status int, reason string)

// FallbackFunc is the last chance to serve a request no pooled backend can:
// either the pool has no backend for the model at all, or
// FilterCandidatesStatus wiped the slate with a 404. Called only when the
// routing posture permits leaving the pool. It returns true when it has
// written a response; the gateway then does nothing more. Optional; a nil
// hook goes straight to 404.
type FallbackFunc func(w http.ResponseWriter, r *http.Request, principal, model string, body []byte) bool

// OnUsageFunc receives the token usage parsed from the tail of a served
// response, once the client has finished reading it. backend is the backend
// that actually delivered (which after a failover is not the one first
// picked). Called from the response-body close path, so it must not block.
type OnUsageFunc func(principal, model, backend string, u openai.Usage, latency time.Duration)

// Refusal is one rejected request: an admission 429 (the principal is at
// its in-flight cap) or a quota 429 (host policy excluded every backend
// that holds the model). Limit is the per-principal admission cap in force
// and InFlight is the principal's live admitted count at refusal time, so
// a usage log can tell "at the cap" from "quota-dry".
type Refusal struct {
	Principal string
	Model     string
	Reason    string
	Status    int
	Limit     int
	InFlight  int
}

// OnRefusalFunc receives every admission and quota refusal, on the request
// path before the 429 is written. It must not block; the gateway never
// retries it.
type OnRefusalFunc func(Refusal)

// ListExtraFunc contributes entries to /v1/models beyond the catalog — a
// host's remote or third-party models, say. Catalog entries win on a name
// collision, so an extra can never shadow a model the pool actually serves.
type ListExtraFunc func(ctx context.Context, principal string) []ModelEntry

// DecorateModelFunc is the per-entry hook for the model metadata the
// resolver's ModelRow does not carry (owner, digest, quantization, cluster
// topology, …). It receives the rows that produced the entry and the entry
// the gateway built; whatever it returns is what the client sees.
type DecorateModelFunc func(rows []resolve.ModelRow, entry ModelEntry) ModelEntry

// Config is everything the gateway needs. Only Catalog, Backend and
// Authorize are required; every scheduler component defaults to a sensible
// private instance and every hook is optional.
type Config struct {
	// Catalog is the model inventory: what each principal can reach.
	Catalog resolve.Catalog

	// Backend resolves a catalog row's backend name to a live Backend.
	// A name with no Backend is skipped as if the row did not exist.
	Backend func(name string) (Backend, bool)

	// Backends enumerates the pool. Used by /health and by anyone
	// wiring a loaded-model poller. Optional; /health reports an empty
	// pool without it.
	Backends func() []Backend

	// Authorize is the one required hook: it turns a request into a
	// principal. Errors are 401.
	Authorize AuthorizeFunc

	// AllowModel, AdmissionLimit and RouteDefault are the per-principal
	// policy lookups. All optional.
	AllowModel     AllowModelFunc
	AdmissionLimit AdmissionLimitFunc
	RouteDefault   RouteDefaultFunc

	// Scheduler and selection components. A nil field gets a private
	// instance of its own, so a zero Config is a working gateway; share
	// them explicitly when several handlers must schedule as one.
	Admitter *sched.Admitter
	Slots    *sched.SlotTable
	Breaker  *sched.Breaker
	Affinity *sched.AffinityCache
	History  *sched.HistoryBuffer
	Jobs     *sched.JobTable
	Metrics  *sched.Metrics
	Capacity *CapacityCache
	Loaded   *LoadedCache

	// Host policy hooks. All optional.
	FilterCandidates       FilterCandidatesFunc
	FilterCandidatesStatus FilterCandidatesStatusFunc
	Fallback               FallbackFunc
	OnUsage                OnUsageFunc
	OnRefusal              OnRefusalFunc
	ListExtra              ListExtraFunc
	DecorateModel          DecorateModelFunc

	// ModifyResponse runs on every upstream response after the gateway
	// has stamped its own headers and installed the tool-call extractor,
	// and before the usage-tail reader wraps the body. Returning an error
	// fails the response the same way a proxy error does.
	ModifyResponse func(*http.Response) error
}

type gateway struct {
	cfg Config
	mux *http.ServeMux
}

// New returns the gateway handler for cfg. Nil scheduler components are
// filled in with private defaults; the returned handler is safe for
// concurrent use and owns no goroutines.
func New(cfg Config) http.Handler {
	if cfg.Admitter == nil {
		cfg.Admitter = sched.NewAdmitter()
	}
	if cfg.Slots == nil {
		cfg.Slots = sched.NewSlotTable()
	}
	if cfg.Breaker == nil {
		// DefaultBreaker rather than a private one: reverse-proxy
		// backends trip DefaultBreaker on a failed attempt, so a
		// private breaker would filter on trips it never sees.
		cfg.Breaker = DefaultBreaker
	}
	if cfg.Affinity == nil {
		cfg.Affinity = sched.NewAffinityCache()
	}
	if cfg.History == nil {
		cfg.History = sched.NewHistoryBuffer()
	}
	if cfg.Jobs == nil {
		cfg.Jobs = sched.NewJobTable()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = sched.NewMetrics(nil)
	}
	if cfg.Capacity == nil {
		cfg.Capacity = NewCapacityCache(DefaultCapacityTTL)
	}
	if cfg.Loaded == nil {
		cfg.Loaded = NewLoadedCache(DefaultLoadedTTL, DefaultLoadedPollInterval)
	}

	g := &gateway{cfg: cfg, mux: http.NewServeMux()}

	// Chat opts IN to session affinity: the message-prefix hash gives a
	// multi-turn conversation a stable session id, so successive turns
	// return to the same backend and reuse its KV cache. Embeddings opt
	// OUT — each call is independent, so routing should optimise purely
	// for load distribution.
	chat := g.inference(openAICodec(ChatCompletionsPath, chatCompletionsBody), true)
	embed := g.inference(openAICodec(EmbeddingsPath, embeddingsBody), false)
	messages := g.inference(anthropicCodec(), true)

	for _, prefix := range []string{"", AliasPrefix} {
		g.mux.Handle("POST "+prefix+ChatCompletionsPath, chat)
		g.mux.Handle("POST "+prefix+EmbeddingsPath, embed)
		g.mux.HandleFunc("GET "+prefix+ModelsPath, g.listModels)
		g.mux.HandleFunc("GET "+prefix+HealthPath, g.health)
	}
	g.mux.Handle("POST "+AnthropicMessagesPath, messages)
	g.mux.Handle("POST "+MessagesPath, messages)
	return g
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }

// routeOutcome discriminates the "nothing to dispatch to" cases so the HTTP
// layer can answer 404 (the model is nowhere) versus 429 (backends exist but
// host policy excluded every one of them).
type routeOutcome string

const (
	routeOK             routeOutcome = ""
	routeNoBackends     routeOutcome = "no_backends"
	routeQuotaExhausted routeOutcome = "quota_exhausted"
	routeFiltered       routeOutcome = "filtered"
	routeCatalogFailed  routeOutcome = "catalog_failed"
)

type routeFailure struct {
	outcome routeOutcome
	status  int
	reason  string
}

// dispatchPick is one resolved routing decision: where to send the request,
// under which model name, and why.
type dispatchPick struct {
	Backend      Backend
	Model        string
	Reason       string
	WasResolved  bool
	OriginalName string
}

// inference is the shared spine for every inference wire format. Request and
// response translation, upstream path, and session-affinity policy arrive as
// arguments; authorization through accounting remains identical.
func (g *gateway) inference(codec inferenceCodec, useAffinity bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		if codec.anthropic && r.Header.Get("Authorization") == "" {
			if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
		}

		principal, ceiling, err := g.cfg.Authorize(r)
		if err != nil {
			codec.writeError(w, http.StatusUnauthorized, errBody(err.Error()))
			return
		}
		principal = strings.TrimSpace(principal)
		if principal == "" {
			codec.writeError(w, http.StatusUnauthorized, errBody("no principal"))
			return
		}

		// Limit-read so a caller cannot exhaust memory with a multi-GB
		// upload before routing has even started.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
		if err != nil {
			codec.writeError(w, http.StatusBadRequest, errBody("read body: "+err.Error()))
			return
		}
		_ = r.Body.Close()

		body, model, stream, err := codec.decode(body)
		if err != nil {
			codec.writeError(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
		if codec.prepare != nil {
			codec.prepare(r, stream)
		}
		if model == "" {
			codec.writeError(w, http.StatusBadRequest, errBody("model is required"))
			return
		}

		// Resolve the auto signal before any access or routing check.
		// ParseAutoSignal strips the @auto suffix, so everything
		// downstream — the allow check and the upstream JSON body
		// included — sees the bare name; the auto behaviour rides on the
		// resolve.Request, not on a magic model string.
		requested := model
		autoReq := resolve.ParseAutoSignal(r, model)
		model = autoReq.Model

		if g.cfg.AllowModel != nil && !g.cfg.AllowModel(principal, model) {
			codec.writeError(w, http.StatusForbidden, map[string]any{
				"error": "this key is not authorized for model " + model,
				"model": model,
			})
			return
		}

		// Routing posture (per-request → per-principal → shipped
		// default) and whether it permits leaving the pool. Only the
		// privacy posture withholds that, and only without a
		// per-request opt-in.
		routePolicy := resolve.ResolveRoutePolicy(r, body, g.routeDefault(principal))
		remoteAllowed := routePolicy != resolve.RoutePrivacy || resolve.RequestAllowsRemote(r, body)

		// Effective service priority: the caller's ceiling, which the
		// request may only make worse. Echoed on the response so the
		// client can see what it actually got.
		priority := EffectiveRequestPriority(r, ceiling)

		// Job admission. The job id glues a multi-turn agentic
		// conversation into one scheduling unit: the first request mints
		// (or adopts the client's) id and runs full admission, later
		// requests with the same id and principal fast-path through the
		// bound slot instead of re-queuing per HTTP call.
		jobID, _ := sched.ResolveJobID(r)
		w.Header().Set(sched.JobIDHeader, jobID)
		jobDone := resolve.IsTruthy(r.Header.Get(sched.JobDoneHeader))
		retryOf := strings.TrimSpace(r.Header.Get(sched.RetryOfHeader))

		_, jobFastPath := g.cfg.Jobs.LookupOrBind(jobID, principal)
		if !jobFastPath {
			out := g.cfg.Admitter.TryAdmit(principal, g.admissionLimit(principal), retryOf)
			switch out.Result {
			case sched.AdmitRateLimited:
				limit := g.admissionLimit(principal)
				g.cfg.Metrics.ObserveDispatch("", "", "rate_limited")
				g.reportRefusal(principal, model, "principal in-flight cap reached",
					http.StatusTooManyRequests, limit, g.cfg.Admitter.InFlight(principal))
				status := sched.ApplyAdmissionHeaders(w, out)
				codec.writeError(w, status, map[string]any{
					"error":         "principal in-flight cap reached",
					"max_in_flight": limit,
					"hint":          "honor Retry-After and slow your concurrent request rate",
				})
				return
			case sched.AdmitOverload:
				g.cfg.Metrics.ObserveDispatch("", "", "overloaded")
				g.cfg.Metrics.ObserveRetryFreeIssued()
				status := sched.ApplyAdmissionHeaders(w, out)
				codec.writeError(w, status, map[string]any{
					"error": "pool queue overloaded",
					"hint":  "retry with " + sched.RetryOfHeader + ": <nonce> for free-retry (no cap or fair-share charge)",
				})
				return
			}
			// Lift the fair-share budget to the current minimum when
			// this principal was idle past the idle TTL — anti-flood
			// on return.
			_ = g.cfg.Admitter.EnterFairBudget(principal)
			defer g.cfg.Admitter.Release(principal)
		}
		admitWait := time.Since(started)

		// Session affinity: a stable conversation id derived from the
		// message prefix, so successive turns of one chat return to the
		// same backend. Embeddings pass "" and fall straight through to
		// capacity-based picking.
		var sessionID string
		if useAffinity {
			sessionID = sched.DeriveSessionID(body)
		}

		ctx := r.Context()
		pick, failure := g.dispatch(ctx, autoReq, principal, sessionID, body)
		if failure.outcome != routeOK {
			g.serveUnroutable(w, r, failure, principal, model, body, remoteAllowed, codec)
			return
		}

		// The dispatched model may differ from the string in the body —
		// substitution picked a stand-in, or the name carried the @auto
		// suffix that must not reach the backend. Patch the body's model
		// field only when it actually differs: the chat path can carry
		// many MB of vision payload, and re-encoding that for a
		// one-field no-op would be pure waste.
		if pick.Model != requested {
			patched, perr := resolve.PatchModelField(body, pick.Model)
			if perr != nil {
				codec.writeError(w, http.StatusInternalServerError, errBody("patch model field: "+perr.Error()))
				return
			}
			body = patched
			model = pick.Model
		}

		primary := pick.Backend
		// Record the (session → backend) binding for the next turn. A
		// no-op when sessionID is empty.
		g.cfg.Affinity.Stick(sessionID, primary.Name(), model)

		// Per-(backend, model) slot. The pool's live view of in-flight
		// can differ from the cached capacity probe between TTLs; when
		// we cannot get a slot we shed with 503 plus a retry-free nonce
		// so a well-behaved client backs off without a fairness penalty.
		slotMax := 0
		if capacity, ok := g.cfg.Capacity.Get(primary.Name()); ok {
			slotMax = capacity.MaxParallel
		}
		if !g.cfg.Slots.Acquire(primary.Name(), model, slotMax) {
			g.cfg.Metrics.ObserveDispatch(primary.Name(), model, "slot_saturated")
			g.cfg.Metrics.ObserveRetryFreeIssued()
			nonce := g.cfg.Admitter.IssueNonce(principal, jobID, 0)
			w.Header().Set("Retry-After", "1")
			w.Header().Set(sched.RetryFreeHeader, nonce)
			codec.writeError(w, http.StatusServiceUnavailable, map[string]any{
				"error": "all reachable (backend, model) slots saturated",
				"model": model,
				"hint":  "retry with " + sched.RetryOfHeader + ": <nonce> for free-retry",
			})
			return
		}
		defer g.cfg.Slots.Release(primary.Name(), model)
		g.cfg.Metrics.ObserveSlotInFlight(primary.Name(), model, int(g.cfg.Slots.InFlight(primary.Name(), model)))
		g.cfg.Metrics.ObserveDispatch(primary.Name(), model, "dispatched")
		g.cfg.Metrics.ObserveResolverStage(pick.Reason, "hit")
		if pick.WasResolved {
			g.cfg.Metrics.ObserveResolvedModel(pick.OriginalName, pick.Model)
		}

		// Bind the chosen (backend, model) to the job so later turns of
		// the same agentic loop fast-path back here.
		g.cfg.Jobs.BindSlot(jobID, primary.Name(), model)

		slate := g.failoverSlate(ctx, principal, model, primary)

		// Queue-state frame for streaming callers. Admission is
		// synchronous today, so the wait is below the threshold and
		// nothing is written; the call is here so a future blocking
		// dispatcher gets the surface for free. When a frame IS written
		// the response is already committed, so failover is off the
		// table — collapse the slate to the primary.
		if sched.WantsSSE(r) {
			snap := g.cfg.Admitter.SnapshotForPrincipal(principal)
			if wrote, _ := sched.WriteQueueStatusSSE(w, nil, snap, admitWait, jobID); wrote {
				slate = []Backend{primary}
			}
		}

		// The backend that actually serves may not be the one we picked
		// — record it as each attempt starts so usage is attributed to
		// the backend that delivered, not the one that died first. The
		// usage callback runs on its own goroutine, hence the atomic.
		var delivered atomic.Value
		delivered.Store(primary.Name())
		for i, b := range slate {
			name := b.Name()
			slate[i] = &recordingBackend{Backend: b, onServe: func() { delivered.Store(name) }}
		}

		modify := g.modifyResponse(modifyArgs{
			upstreamPath: codec.upstreamPath,
			codec:        codec,
			principal:    principal,
			jobID:        jobID,
			jobDone:      jobDone,
			priority:     priority,
			model:        model,
			autoModel:    autoReq.Model,
			body:         body,
			started:      started,
			admitWait:    admitWait,
			pick:         pick,
			delivered:    func() string { name, _ := delivered.Load().(string); return name },
		})

		served := &byteWriteCounter{ResponseWriter: w}
		chosen, _, status := ServeWithFailover(served, r, slate, body, modify)
		if served.bytesWritten == 0 && status >= http.StatusBadRequest {
			codec.writeError(w, status, errBody("upstream request failed"))
		}
		if chosen != nil && chosen.Name() != primary.Name() {
			// Rebind the affinity to the backend that actually
			// delivered, so the next turn pins to the known-working
			// one rather than the primary that died on first byte.
			g.cfg.Affinity.Stick(sessionID, chosen.Name(), model)
			g.cfg.Metrics.ObserveDispatch(chosen.Name(), model, "failover")
		}
	}
}

// recordingBackend notes that an attempt started before delegating. It is
// how the response callbacks learn which backend is on the wire.
type recordingBackend struct {
	Backend
	onServe func()
}

func (b *recordingBackend) Serve(w http.ResponseWriter, r *http.Request, body []byte, modify func(*http.Response) error) Attempt {
	b.onServe()
	return b.Backend.Serve(w, r, body, modify)
}

// modifyArgs is the request-scoped state the response callback closes over.
type modifyArgs struct {
	upstreamPath string
	codec        inferenceCodec
	principal    string
	jobID        string
	jobDone      bool
	priority     int
	model        string
	autoModel    string
	body         []byte
	started      time.Time
	admitWait    time.Duration
	pick         dispatchPick
	delivered    func() string
}

// modifyResponse builds the upstream-response callback: the gateway's own
// response headers, tool-call extraction, and the usage tail that feeds
// accounting, fair-share charging and the resolver's history buffer.
func (g *gateway) modifyResponse(a modifyArgs) func(*http.Response) error {
	return func(resp *http.Response) error {
		if a.pick.Reason != "" {
			resp.Header.Set(resolve.ResolvedModelHeader, a.pick.Model)
			resp.Header.Set(resolve.ResolvedReasonHeader, a.pick.Reason)
		}
		// The job id is already on the ResponseWriter; the proxy ADDS
		// upstream headers to it, so setting it here too would send the
		// client the same value twice.
		resp.Header.Del(sched.JobIDHeader)
		resp.Header.Set(PriorityHeader, strconv.Itoa(a.priority))

		// Some models emit tool invocations as JSON text in the message
		// content instead of populating the structured tool_calls field.
		// The extractor pattern-matches that content and rewrites the
		// response into the spec shape, so every OpenAI-compatible
		// client gets a structured call rather than a blob it cannot
		// act on. Chat only — embeddings have no tool-call surface.
		if a.upstreamPath == ChatCompletionsPath && resp.StatusCode == http.StatusOK {
			resp.Body = openai.ApplyToolCallExtractor(resp)
		}

		// Queue state, snapshotted here so the figures describe what the
		// caller actually saw rather than what was true at decode time.
		snap := g.cfg.Admitter.SnapshotForPrincipal(a.principal)
		sched.StampQueueHeaders(resp.Header, snap, int(a.admitWait/time.Millisecond))

		if g.cfg.ModifyResponse != nil {
			if err := g.cfg.ModifyResponse(resp); err != nil {
				return err
			}
		}

		status := resp.StatusCode
		resp.Body = openai.NewAuditTailReader(resp.Body, openai.UsageTailSize, func(tail []byte, _ error) {
			usage := openai.ParseUsage(tail)
			if g.cfg.OnUsage != nil {
				g.cfg.OnUsage(a.principal, a.model, a.delivered(), usage, time.Since(a.started))
			}
			// Charge fair-share against the principal and the job at
			// completion. TotalTokens is zero for a stream that did not
			// ask for usage; fairness still works on whatever volume we
			// can observe.
			if usage.TotalTokens > 0 {
				g.cfg.Admitter.ChargeVTC(a.principal, int64(usage.TotalTokens))
				g.cfg.Jobs.AddTokensCharged(a.jobID, int64(usage.TotalTokens))
			} else {
				g.cfg.Jobs.MarkActive(a.jobID)
			}
			// When auto-classification picked the model, remember the
			// successful resolution so a similar future prompt
			// fast-paths to the same one.
			if resolve.IsAutoModelName(a.autoModel) && status < 400 {
				key := resolve.PromptPrefixKey(resolve.ExtractFirstUserPrompt(a.body))
				g.cfg.History.Record(key, a.model, 0, nil, true)
			}
			if a.jobDone {
				g.cfg.Jobs.Release(a.jobID)
			}
		})
		if a.codec.adapt != nil {
			if err := a.codec.adapt(resp, strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")); err != nil {
				return err
			}
		}
		return nil
	}
}

// dispatch walks the resolver's candidate models in priority order and
// returns the first (backend, model) pair that can serve. On failure the
// outcome says whether the model was nowhere to be found or whether host
// policy excluded the backends that had it.
func (g *gateway) dispatch(ctx context.Context, req resolve.Request, principal, sessionID string, body []byte) (dispatchPick, routeFailure) {
	names, err := resolve.CandidateModelsErr(ctx, g.cfg.Catalog, req, principal,
		func(backend, model string) bool { return g.cfg.Loaded.HasLoaded(backend, model) },
		historyAdapter{g.cfg.History}, body)
	if err != nil {
		return dispatchPick{}, routeFailure{outcome: routeCatalogFailed, reason: err.Error()}
	}
	if len(names) == 0 {
		return dispatchPick{}, routeFailure{outcome: routeNoBackends}
	}

	// Track the worst outcome across attempts so an empty result reports
	// exclusion (429) in preference to absence (404).
	var worst routeFailure
	for i, name := range names {
		candidates, failure := g.backendsFor(ctx, principal, name)
		if failure.outcome == routeCatalogFailed {
			return dispatchPick{}, failure
		}
		if backend := g.pickBackend(ctx, name, sessionID, candidates); backend != nil {
			pick := dispatchPick{
				Backend:      backend,
				Model:        name,
				OriginalName: req.Model,
				WasResolved:  req.AutoEnabled && name != req.Model,
			}
			switch {
			case !req.AutoEnabled || name == req.Model:
				pick.Reason = resolve.ReasonExactMatch
			case req.Mode == resolve.ModeBestWarm && i == 0:
				pick.Reason = resolve.ReasonSubstitutedWarm
			default:
				pick.Reason = resolve.ReasonSubstitutedFailover
			}
			return pick, routeFailure{}
		}
		if failure.outcome != routeOK && failure.outcome != routeNoBackends && worst.outcome == routeOK {
			worst = failure
		}
		// Never substitute across an exact match the caller forbade.
		if !req.AutoEnabled {
			break
		}
	}
	if worst.outcome == routeOK {
		worst.outcome = routeNoBackends
	}
	return dispatchPick{OriginalName: req.Model}, worst
}

// backendsFor returns the live backends exposing model to principal, in
// catalog order, or a failure describing why none can be used.
func (g *gateway) backendsFor(ctx context.Context, principal, model string) (backends []Backend, failure routeFailure) {
	if g.cfg.Catalog == nil || g.cfg.Backend == nil {
		return nil, routeFailure{outcome: routeNoBackends}
	}
	rows, err := catalogRows(ctx, g.cfg.Catalog, principal)
	if err != nil {
		return nil, routeFailure{outcome: routeCatalogFailed, reason: err.Error()}
	}
	seen := map[string]struct{}{}
	names := make([]string, 0, 4)
	for _, row := range rows {
		if row.Name != model {
			continue
		}
		if _, dup := seen[row.Backend]; dup {
			continue
		}
		seen[row.Backend] = struct{}{}
		names = append(names, row.Backend)
	}
	if len(names) == 0 {
		return nil, routeFailure{outcome: routeNoBackends}
	}
	legacyExcluded := false
	if g.cfg.FilterCandidatesStatus != nil {
		kept, status, reason := g.cfg.FilterCandidatesStatus(ctx, principal, model, names)
		if len(kept) == 0 {
			return nil, routeFailure{
				outcome: routeFiltered,
				status:  filterCandidatesStatus(status),
				reason:  strings.TrimSpace(reason),
			}
		}
		names = kept
	} else if g.cfg.FilterCandidates != nil {
		kept := g.cfg.FilterCandidates(ctx, principal, model, names)
		legacyExcluded = len(kept) < len(names)
		if len(kept) == 0 {
			return nil, routeFailure{outcome: routeQuotaExhausted}
		}
		names = kept
	}
	backends = make([]Backend, 0, len(names))
	for _, name := range names {
		if backend, ok := g.cfg.Backend(name); ok && backend != nil {
			backends = append(backends, backend)
		}
	}
	if len(backends) == 0 {
		if legacyExcluded {
			return nil, routeFailure{outcome: routeQuotaExhausted}
		}
		return nil, routeFailure{outcome: routeNoBackends}
	}
	return backends, routeFailure{}
}

func filterCandidatesStatus(status int) int {
	switch status {
	case http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return status
	default:
		return http.StatusTooManyRequests
	}
}

// pickBackend applies the selection tiers, first applicable wins:
//
//  1. Session affinity — an in-TTL sticky binding, when the bound backend
//     is still in the candidate set.
//  2. Loaded-model preference — a backend with the model already resident
//     saves seconds of load latency, which dominates any in-flight delta.
//     When any candidate is warm, rank by capacity within just that bucket.
//  3. Capacity-aware least-loaded across the whole set.
func (g *gateway) pickBackend(ctx context.Context, model, sessionID string, candidates []Backend) Backend {
	if len(candidates) == 0 {
		return nil
	}
	if sessionID != "" {
		if sticky, _, ok := g.cfg.Affinity.Get(sessionID); ok {
			for _, backend := range candidates {
				if backend.Name() == sticky {
					return backend
				}
			}
		}
	}
	if loaded, _ := g.cfg.Loaded.PartitionLoaded(model, candidates); len(loaded) > 0 {
		return g.cfg.Capacity.PickLeastLoaded(ctx, loaded)
	}
	return g.cfg.Capacity.PickLeastLoaded(ctx, candidates)
}

// failoverSlate re-derives the ordered attempt list for the dispatched
// model: the primary always leads, then the next-best candidate by the same
// warm-first ranking, minus anything the breaker is cooling. Bounded so the
// tail-latency cost of a retry stays capped.
func (g *gateway) failoverSlate(ctx context.Context, principal, model string, primary Backend) []Backend {
	candidates, _ := g.backendsFor(ctx, principal, model)
	loaded, others := g.cfg.Loaded.PartitionLoaded(model, candidates)
	ranked := PickCandidatesForFailover(append(append([]Backend{}, loaded...), others...), g.cfg.Breaker)

	// Without the explicit lead, an expired warm-cache entry could
	// relegate the primary and start the retry loop on a backend we had
	// not yet attempted.
	slate := []Backend{primary}
	for _, backend := range ranked {
		if backend == nil || backend.Name() == primary.Name() {
			continue
		}
		slate = append(slate, backend)
		if len(slate) >= 1+failoverRetryBudget {
			break
		}
	}
	return slate
}

// serveUnroutable answers a request no pooled backend can serve: the host's
// fallback first when the posture permits leaving the pool, then the status
// the routing outcome calls for.
func (g *gateway) serveUnroutable(w http.ResponseWriter, r *http.Request, failure routeFailure, principal, model string, body []byte, remoteAllowed bool, codec inferenceCodec) {
	switch failure.outcome {
	case routeCatalogFailed:
		writeCatalogUnavailable(w, codec, failure.reason)
	case routeFiltered:
		reason := failure.reason
		if reason == "" {
			reason = "every backend for " + model + " was excluded by host policy"
		}
		if failure.status == http.StatusTooManyRequests {
			limit := g.admissionLimit(principal)
			g.cfg.Metrics.ObserveDispatch("", model, "quota_exhausted")
			g.reportRefusal(principal, model, reason, failure.status, limit, g.cfg.Admitter.InFlight(principal))
		}
		// A 404 wipe says the model is nowhere in the pool, which is
		// exactly the routeNoBackends story — so it escalates to the
		// host's fallback the same way. A 429 or 503 wipe does not: the
		// backends exist and a later retry is the honest advice, so
		// leaving the pool would silently change where the request ran.
		if failure.status == http.StatusNotFound && remoteAllowed && g.cfg.Fallback != nil {
			if g.cfg.Fallback(w, r, principal, model, body) {
				return
			}
		}
		codec.writeError(w, failure.status, map[string]any{"error": reason, "model": model})
	case routeQuotaExhausted:
		// Every eligible backend was excluded by host policy. 429 is the
		// only answer that tells a well-behaved client to back off and
		// that a later retry will likely work — a 404 would read as
		// "this model does not exist."
		reason := "every backend for " + model + " is over its shared-usage budget"
		limit := g.admissionLimit(principal)
		g.cfg.Metrics.ObserveDispatch("", model, "quota_exhausted")
		g.reportRefusal(principal, model, reason, http.StatusTooManyRequests, limit, g.cfg.Admitter.InFlight(principal))
		codec.writeError(w, http.StatusTooManyRequests, map[string]any{
			"error": reason,
			"model": model,
		})
	case routeNoBackends:
		if remoteAllowed && g.cfg.Fallback != nil {
			if g.cfg.Fallback(w, r, principal, model, body) {
				return
			}
		}
		msg := "no reachable backend has model " + model
		if !remoteAllowed {
			msg = "no local backend has model " + model +
				"; remote routing is disabled by your privacy route policy" +
				" (set " + resolve.RouteHeader + ": " + resolve.RouteLocalFirst +
				", or pass allow_cloud)"
		}
		codec.writeError(w, http.StatusNotFound, map[string]any{"error": msg, "model": model})
	default:
		codec.writeError(w, http.StatusInternalServerError, errBody("router returned no backend and no reason"))
	}
}

func (g *gateway) admissionLimit(principal string) int {
	if g.cfg.AdmissionLimit != nil {
		if n := g.cfg.AdmissionLimit(principal); n > 0 {
			return n
		}
	}
	return DefaultMaxInFlight
}

// reportRefusal delivers one 429 to the host's refusal hook, if any. A nil
// hook (or a hook that panics) must never fail the refusal itself.
func (g *gateway) reportRefusal(principal, model, reason string, status, limit, inFlight int) {
	if g.cfg.OnRefusal == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		g.cfg.OnRefusal(Refusal{
			Principal: principal,
			Model:     model,
			Reason:    reason,
			Status:    status,
			Limit:     limit,
			InFlight:  inFlight,
		})
	}()
}

func (g *gateway) routeDefault(principal string) string {
	if g.cfg.RouteDefault == nil {
		return ""
	}
	return g.cfg.RouteDefault(principal)
}

// historyAdapter presents the scheduler's history buffer as the resolver's
// PromptHistory seam. The two packages deliberately do not know each other;
// the gateway is where they meet.
type historyAdapter struct{ buf *sched.HistoryBuffer }

func (a historyAdapter) Lookup(key string) (resolve.PromptResolution, bool) {
	if a.buf == nil {
		return resolve.PromptResolution{}, false
	}
	rec, ok := a.buf.Lookup(key)
	if !ok {
		return resolve.PromptResolution{}, false
	}
	return resolve.PromptResolution{Tier: rec.Tier, Domains: rec.Domains, Success: rec.Success}, true
}

// chatRequestEnvelope is the minimal slice of the chat-completion request
// the gateway reads: just enough to route. Everything else (messages, tools,
// vision parts, temperature, response_format, …) passes through untouched.
type chatRequestEnvelope struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// chatCompletionsBody extracts the model name from a chat-completion body.
// Tolerant of every other field; only "model" is read.
func chatCompletionsBody(body []byte) (string, error) {
	var env chatRequestEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", err
	}
	return strings.TrimSpace(env.Model), nil
}

// embeddingsBody extracts the model name from an embeddings body. Same
// envelope shape as chat.
func embeddingsBody(body []byte) (string, error) {
	var env struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", err
	}
	return strings.TrimSpace(env.Model), nil
}

func errBody(msg string) map[string]any { return map[string]any{"error": msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
