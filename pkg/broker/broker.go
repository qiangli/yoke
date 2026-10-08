// Package broker is the host's one door to its model capacity (Sprint 302):
// a single process per host that every shell, subshell and agent goes
// through, whether the model is a local one on bashy's own Ollama engine or a
// fleet agent served by cligw. It schedules the exclusive local device (a
// run queue with priority classes), scopes sessions by shell lineage, freezes
// sticky identities for reproducible runs, and records every request.
//
// Design of record: dhnt docs/bashy-shared-model-memory-scheduler-design.md.
package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/cligw"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// DefaultPort is the door's port: 24556, "AILLM" on a telephone keypad (as
// 22749 is "BASHY" for the apps console). Below every OS's ephemeral range.
const DefaultPort = door.DefaultPort

// Request headers the broker reads, beyond the sticky header.
const (
	SessionHeader  = "X-Bashy-Session"
	ClassHeader    = "X-Bashy-Class"
	IdentityHeader = "X-Bashy-Identity"
	BackendHeader  = "X-Bashy-Backend"
	WaitHeader     = "X-Bashy-Queue-Wait-Ms"
)

// Backend names as they appear in records and headers.
const (
	BackendLocal = "ollama-local"
	BackendCLI   = "cligw"
)

// AgentInfo is what the broker needs to know about a fleet agent to freeze it.
type AgentInfo struct {
	Name        string
	Tool        string
	Model       string
	VendorModel string
	Provider    string
	Kind        string
	Warm        string
	Effort      string // declared reasoning effort ("" = the tool's default)
	Band        int
}

// AgentLookup is an optional CLIBackend capability: the agent's binding as
// it stands now, so a frozen identity's declared effort can be re-checked on
// every use. A backend without it is not re-checked.
type AgentLookup interface {
	LookupAgent(name string) (AgentInfo, bool)
}

// CLIBackend is the cligw surface: an http.Handler for the OpenAI/Anthropic
// routes (it authenticates with its own bearer token, which the broker
// supplies), plus agent resolution for sticky bindings.
type CLIBackend interface {
	http.Handler
	Token() string
	ResolveAgent(ctx context.Context, model, filter string) (AgentInfo, error)
}

// Options configures a Broker.
type Options struct {
	// Engine is the local model server. Nil = no local backend.
	Engine Engine
	// CLI serves fleet agents. Nil = no vendor seats.
	CLI CLIBackend
	// Token is the owner's bearer token (required).
	Token string
	// Principal names the owner (default "owner").
	Principal string
	// QueueLimit bounds the local run queue (0 = DefaultQueueLimit).
	QueueLimit int
	// MemoryBytes is the host's physical memory for admission (0 = unknown,
	// no memory refusal). Headroom is the usable fraction (default 0.85).
	MemoryBytes uint64
	Headroom    float64
	// EngineContext is the default context length the engine was started
	// with; it is part of a local identity when a binding names none.
	EngineContext int
	// StateDir holds sticky.json and audit.jsonl ("" = memory only).
	StateDir string
	// ToolVersion reports a CLI tool's version for identities (nil = none).
	ToolVersion func(tool string) string
	// Now is the clock (tests).
	Now func() time.Time
}

// Broker is the door.
type Broker struct {
	opts   Options
	engine engineClient
	local  bool
	device *Device
	sticky *stickyStore
	audit  *auditLog
	tracer trace.Tracer

	// stickyW holds one warm CLI per bind=worker/reset=none binding, keyed
	// like the sticky store (principal + "\x00" + key). Sessions retire when
	// their binding is deleted or expires (onEvict) or when a turn fails.
	stickyWMu sync.Mutex
	stickyW   map[string]*stickyEntry

	tagMu   sync.Mutex
	tags    map[string]engineModel // canonical name -> model
	tagsAt  time.Time
	derived map[string]bool
}

// New builds a broker and starts its engine.
func New(ctx context.Context, opts Options) (*Broker, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("broker: an owner token is required")
	}
	if opts.Principal == "" {
		opts.Principal = "owner"
	}
	if opts.Headroom <= 0 || opts.Headroom > 1 {
		opts.Headroom = 0.85
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	b := &Broker{
		opts:    opts,
		device:  NewDevice(opts.QueueLimit),
		tracer:  otel.Tracer("bashy/broker"),
		derived: map[string]bool{},
		stickyW: map[string]*stickyEntry{},
	}
	var stickyPath, auditPath string
	if opts.StateDir != "" {
		stickyPath = filepath.Join(opts.StateDir, "sticky.json")
		auditPath = filepath.Join(opts.StateDir, "audit.jsonl")
	}
	b.sticky = newStickyStore(stickyPath, opts.Now)
	b.sticky.onEvict = b.evictStickyWorker
	b.audit = newAuditLog(auditPath)
	if opts.Engine != nil {
		base, err := opts.Engine.Start(ctx)
		if err != nil {
			return nil, err
		}
		b.engine = engineClient{base: base}
		b.local = true
		b.device.OnSwitch = b.onPrincipalSwitch
	}
	return b, nil
}

// Close stops the engine.
func (b *Broker) Close(ctx context.Context) error {
	if b.opts.Engine != nil {
		return b.opts.Engine.Stop(ctx)
	}
	return nil
}

// Device exposes the run queue (live view, tests).
func (b *Broker) Device() *Device { return b.device }

// onPrincipalSwitch evicts resident models before another principal's work
// runs on the device, so no prefix/KV cache hit ever crosses principals (Q8).
// This is content isolation, not timing isolation: shared queue contention,
// model residency and eviction/reload latency can reveal another scope's
// activity. Responses are not constant-time; callers requiring timing isolation
// need separate execution resources.
func (b *Broker) onPrincipalSwitch(ctx context.Context, from, to string) error {
	models, err := b.engine.ps(ctx)
	if err != nil {
		return fmt.Errorf("cache isolation: list resident models: %w", err)
	}
	for _, m := range models {
		if err := b.engine.unload(ctx, m.Name); err != nil {
			return fmt.Errorf("cache isolation: unload %s: %w", m.Name, err)
		}
	}
	return nil
}

// ---- request context -------------------------------------------------------

type ctxKey int

const unixOwnerKey ctxKey = 1

// reqInfo is everything the pipeline learned about a request.
type reqInfo struct {
	principal string
	session   string
	class     Class
	params    map[string]string
	started   time.Time
	rec       Record
	finished  bool
}

// splitParams peels the /k/<token>, /s/<session> and /sticky/<key> pairs a
// base-URL-only client can carry in its URL, returning them and the rest.
func splitParams(p string) (map[string]string, string) {
	params := map[string]string{}
	for {
		trimmed := strings.TrimPrefix(p, "/")
		name, rest, ok := strings.Cut(trimmed, "/")
		if !ok || (name != "k" && name != "s" && name != "sticky") {
			return params, p
		}
		val, after, _ := strings.Cut(rest, "/")
		if v, err := url.PathUnescape(val); err == nil {
			val = v
		}
		params[name] = val
		p = "/" + after
	}
}

func (b *Broker) authorize(r *http.Request, pathToken string) (string, error) {
	if owner, unixPeer := r.Context().Value(unixOwnerKey).(bool); unixPeer {
		if !owner {
			return "", errors.New("unix socket peer is not the owner")
		}
		return b.opts.Principal, nil
	}
	presented := pathToken
	if presented == "" {
		presented = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	if presented == "" {
		presented = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if presented == "" {
		return "", errors.New("missing token: use the owner-only socket, a bearer token, or the /k/<token> URL form (see `bashy llm env`)")
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(b.opts.Token)) != 1 {
		return "", errors.New("invalid token (see `bashy llm env`)")
	}
	return b.opts.Principal, nil
}

// ---- HTTP surface ----------------------------------------------------------

// ServeHTTP is the door.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	params, rest := splitParams(r.URL.Path)
	ri := &reqInfo{params: params, started: b.opts.Now()}
	ri.rec = Record{Time: ri.started, Method: r.Method, Path: rest, Class: ClassInteractive.String()}
	cw := newCaptureWriter(w)
	w = cw
	defer func() {
		if !ri.finished {
			if cw.status == 0 {
				cw.status = http.StatusOK
			}
			ri.rec.PromptTok, ri.rec.OutputTok = parseTokens(cw.tail)
			b.finish(ri, cw.status, 0, "")
		}
	}()
	principal, err := b.authorize(r, params["k"])
	if err != nil {
		writeErr(w, rest, http.StatusUnauthorized, err.Error())
		return
	}
	ri.principal = principal
	ri.rec.Principal = principal
	ri.session = params["s"]
	if ri.session == "" {
		ri.session = strings.TrimSpace(r.Header.Get(SessionHeader))
	}
	ri.rec.Session = ri.session
	if err := ValidateSession(ri.session); err != nil {
		writeErr(w, rest, http.StatusBadRequest, err.Error())
		return
	}
	// The rest of the pipeline sees a plain request: no URL parameters, no
	// broker token (a delegate gets its own credentials).
	r = r.Clone(r.Context())
	r.URL.Path, r.URL.RawPath = rest, ""
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")

	switch {
	case strings.HasPrefix(rest, "/cligw/"):
		// cligw's own surface (its /health for `llm pools`, its routes).
		r.URL.Path = strings.TrimPrefix(rest, "/cligw")
		b.delegateCLI(w, r, ri, nil)
	case rest == "/health" || rest == "/":
		b.serveHealth(w, r)
	case rest == "/v1/sticky" || strings.HasPrefix(rest, "/v1/sticky/"):
		b.serveStickyAPI(w, r, ri)
	case strings.HasPrefix(rest, "/v1/sessions/"):
		b.serveTurns(w, r, ri)
	case r.Method == http.MethodGet && (rest == "/v1/models" || rest == "/openai/v1/models"):
		b.serveModels(w, r, ri)
	case strings.HasPrefix(rest, "/api/"):
		b.serveNative(w, r, ri)
	case r.Method == http.MethodPost && isInferencePath(rest):
		b.serveInference(w, r, ri)
	default:
		if b.opts.CLI != nil {
			b.delegateCLI(w, r, ri, nil)
			return
		}
		writeErr(w, rest, http.StatusNotFound, "no such route: "+rest)
	}
}

func isInferencePath(p string) bool {
	p = strings.TrimPrefix(p, "/openai")
	switch p {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/messages", "/anthropic/v1/messages":
		return true
	}
	return false
}

// nativeDevicePaths run on the device; every other /api/ route is a
// management call proxied straight through.
var nativeDevicePaths = map[string]bool{
	"/api/generate": true, "/api/chat": true, "/api/embed": true, "/api/embeddings": true,
}

func (b *Broker) serveNative(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	if !b.local {
		writeErr(w, r.URL.Path, http.StatusServiceUnavailable, "this broker has no local model engine")
		return
	}
	if !(r.Method == http.MethodPost && nativeDevicePaths[r.URL.Path]) {
		b.proxyEngine(w, r, nil)
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadRequest, err.Error())
		return
	}
	b.dispatch(w, r, ri, body, true)
}

func (b *Broker) serveInference(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadRequest, err.Error())
		return
	}
	b.dispatch(w, r, ri, body, false)
}

// dispatch is the inference spine: sticky → backend choice → admission →
// device → proxy → record.
func (b *Broker) dispatch(w http.ResponseWriter, r *http.Request, ri *reqInfo, body []byte, native bool) {
	ctx, span := b.tracer.Start(r.Context(), "broker.request",
		trace.WithAttributes(attribute.String("bashy.path", r.URL.Path), attribute.String("bashy.session", ri.session)))
	defer span.End()
	r = r.WithContext(ctx)

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, r.URL.Path, http.StatusBadRequest, "request body is not a JSON object")
		return
	}
	model := jsonString(payload["model"])

	// A slash= request RUNS a vendor tool command (cligw slash.go); it is not
	// a completion from a frozen identity. Refuse it on a sticky binding so a
	// benchmark identity only ever serves completions (Sprint #324 S5).
	slash := slashOf(r.Header.Get(cligw.FilterHeader))
	if slash != "" && (ri.params["sticky"] != "" || strings.TrimSpace(r.Header.Get(StickyHeader)) != "") {
		writeErr(w, r.URL.Path, http.StatusBadRequest, fmt.Sprintf(
			"sticky: slash=%s runs a tool command and is not served from a sticky binding (identities serve completions only); send it without %s or /sticky/<key>",
			slash, StickyHeader))
		return
	}

	binding, spec, err := b.stickyFor(ctx, r, ri, payload, model)
	if err != nil {
		writeStickyErr(w, r.URL.Path, err)
		return
	}
	defClass := ClassInteractive
	if binding != nil {
		defClass = ClassBatch
	}
	class, err := ParseClass(r.Header.Get(ClassHeader), defClass)
	if err != nil {
		writeErr(w, r.URL.Path, http.StatusBadRequest, err.Error())
		return
	}
	ri.class = class
	ri.rec = Record{Time: ri.started, Principal: ri.principal, Session: ri.session, Class: class.String(),
		Method: r.Method, Path: r.URL.Path}

	backend := BackendLocal
	if binding != nil {
		backend = binding.Identity.Backend
		model = binding.Identity.Model
		if backend == BackendCLI {
			model = binding.Identity.Agent
		}
	} else if _, ok := b.localModel(ctx, model); !ok {
		backend = BackendCLI
	}
	if slash != "" && backend == BackendLocal {
		writeErr(w, r.URL.Path, http.StatusBadRequest, fmt.Sprintf(
			"slash=%s runs a fleet agent's tool command; model %q is a local engine model — name a band, agent or `auto`", slash, model))
		return
	}
	if native && backend != BackendLocal {
		writeErr(w, r.URL.Path, http.StatusNotFound, fmt.Sprintf("model %q is not a local model on this host's engine", model))
		return
	}

	if binding != nil {
		if err := b.checkIdentity(ctx, binding); err != nil {
			writeStickyErr(w, r.URL.Path, err)
			return
		}
		var messages []json.RawMessage
		if binding.Spec.Reset == ResetNone {
			_ = json.Unmarshal(payload["messages"], &messages)
			if messages == nil {
				messages = []json.RawMessage{}
			}
		}
		use, err := b.sticky.use(ri.principal, ri.session, binding.Spec.Key, messages)
		if err != nil {
			writeStickyErr(w, r.URL.Path, err)
			return
		}
		ri.rec.Sticky, ri.rec.Identity, ri.rec.Use = binding.Spec.Key, binding.Digest, use
		useText := strconv.Itoa(use)
		if binding.Spec.Uses > 0 {
			useText += "/" + strconv.Itoa(binding.Spec.Uses)
		}
		w.Header().Set(StickyHeader, fmt.Sprintf("%s; identity=%s; use=%s", binding.Spec.Key, ShortDigest(binding.Digest), useText))
		w.Header().Set(IdentityHeader, binding.Digest)
		span.SetAttributes(attribute.String("bashy.sticky", binding.Spec.Key), attribute.String("bashy.identity", binding.Digest))
		_ = spec
	}
	w.Header().Set(BackendHeader, backend)
	ri.rec.Backend = backend
	span.SetAttributes(attribute.String("bashy.backend", backend), attribute.String("bashy.class", class.String()))

	if backend == BackendCLI {
		if b.opts.CLI == nil {
			writeErr(w, r.URL.Path, http.StatusNotFound, fmt.Sprintf("model %q is not a local model and this broker serves no fleet agents", model))
			return
		}
		// One warm CLI per binding: reset=none implies bind=worker, and the
		// turn runs on the binding's held session with only the new messages.
		// bind=worker with reset=each needs no conversation, so every turn is
		// an independent one-shot below.
		if binding != nil && binding.Spec.Bind == BindWorker && binding.Spec.Reset == ResetNone {
			b.serveStickyWorker(w, r, ri, binding, payload)
			return
		}
		if binding != nil {
			payload["model"], _ = json.Marshal(model)
			body, _ = json.Marshal(payload)
		}
		ri.rec.Model = model
		b.delegateCLI(w, r, ri, body)
		return
	}
	b.serveLocal(w, r, ri, payload, model, binding, native)
}

// serveLocal runs one request on the exclusive device.
func (b *Broker) serveLocal(w http.ResponseWriter, r *http.Request, ri *reqInfo, payload map[string]json.RawMessage, model string, binding *Binding, native bool) {
	ctx := r.Context()
	m, ok := b.localModel(ctx, model)
	if !ok {
		b.finish(ri, http.StatusNotFound, 0, fmt.Sprintf("model %q not found", model))
		writeErr(w, r.URL.Path, http.StatusNotFound, fmt.Sprintf("model %q is not on this host's engine (bashy ollama pull %s)", model, model))
		return
	}
	ri.rec.Model, ri.rec.ModelDigest = m.Name, m.Digest
	if b.opts.MemoryBytes > 0 && m.Size > 0 {
		need := uint64(float64(m.Size) * 1.1)
		have := uint64(float64(b.opts.MemoryBytes) * b.opts.Headroom)
		if need > have {
			msg := fmt.Sprintf("insufficient memory: %s needs about %.1f GB, this host allows %.1f GB for models", m.Name, gb(need), gb(have))
			b.finish(ri, http.StatusServiceUnavailable, 0, msg)
			w.Header().Set("X-Bashy-Refusal", "memory")
			writeErr(w, r.URL.Path, http.StatusServiceUnavailable, msg)
			return
		}
	}

	options := map[string]any{}
	if binding != nil {
		for k, v := range binding.Identity.Options {
			options[k] = v
		}
	}
	target := m.Name
	if native {
		// Native API: options travel in the body.
		var reqOpts map[string]any
		_ = json.Unmarshal(payload["options"], &reqOpts)
		if reqOpts == nil {
			reqOpts = map[string]any{}
		}
		for k, v := range options {
			reqOpts[k] = v
		}
		if len(reqOpts) > 0 {
			payload["options"], _ = json.Marshal(reqOpts)
		}
		ri.rec.Options = reqOpts
	} else {
		// OpenAI surface: temperature/seed/top_p have fields; anything else
		// (num_ctx, top_k) goes into a derived model sharing the weights.
		derivedParams := map[string]any{}
		for k, v := range options {
			switch k {
			case "temperature", "seed", "top_p":
				payload[k], _ = json.Marshal(v)
			case "num_predict":
				payload["max_tokens"], _ = json.Marshal(v)
			default:
				derivedParams[k] = v
			}
		}
		if len(derivedParams) > 0 {
			name, err := b.derivedModel(ctx, m.Name, derivedParams)
			if err != nil {
				b.finish(ri, http.StatusBadGateway, 0, err.Error())
				writeErr(w, r.URL.Path, http.StatusBadGateway, err.Error())
				return
			}
			target = name
		}
		if len(options) > 0 {
			ri.rec.Options = options
		}
	}
	payload["model"], _ = json.Marshal(target)
	body, _ := json.Marshal(payload)

	release, wait, err := b.device.Acquire(ctx, ri.class, storeKey(ri.principal, ri.session))
	w.Header().Set(WaitHeader, strconv.FormatInt(wait.Milliseconds(), 10))
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int64("bashy.queue_wait_ms", wait.Milliseconds()),
		attribute.String("bashy.model", m.Name), attribute.String("bashy.model_digest", m.Digest))
	if err != nil {
		if errors.Is(err, ErrQueueFull) {
			b.finish(ri, http.StatusServiceUnavailable, wait, err.Error())
			w.Header().Set("Retry-After", "5")
			w.Header().Set("X-Bashy-Refusal", "queue-full")
			writeErr(w, r.URL.Path, http.StatusServiceUnavailable, "the local model queue is full; retry later")
			return
		}
		status := http.StatusServiceUnavailable
		if ctx.Err() != nil {
			status = 499
		}
		b.finish(ri, status, wait, err.Error())
		writeErr(w, r.URL.Path, status, err.Error())
		return
	}
	defer release()
	cw := newCaptureWriter(w)
	b.proxyEngine(cw, r, body)
	ri.rec.PromptTok, ri.rec.OutputTok = parseTokens(cw.tail)
	status := cw.status
	if ctx.Err() != nil {
		status = 499 // the client hung up; whatever the engine said went nowhere
	}
	b.finish(ri, status, wait, "")
}

func (b *Broker) finish(ri *reqInfo, status int, wait time.Duration, errMsg string) {
	if ri.finished {
		return
	}
	ri.finished = true
	ri.rec.Status = status
	ri.rec.WaitMS = wait.Milliseconds()
	ri.rec.WallMS = b.opts.Now().Sub(ri.started).Milliseconds()
	ri.rec.Error = errMsg
	b.audit.append(ri.rec)
}

func gb(n uint64) float64 { return float64(n) / (1 << 30) }

// proxyEngine forwards r (with body, when given) to the engine.
func (b *Broker) proxyEngine(w http.ResponseWriter, r *http.Request, body []byte) {
	target, _ := url.Parse(b.engine.base)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeErr(w, r.URL.Path, http.StatusBadGateway, "local engine: "+err.Error())
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	proxy.ServeHTTP(w, r)
}

// delegateCLI hands a request to cligw with cligw's own bearer token.
func (b *Broker) delegateCLI(w http.ResponseWriter, r *http.Request, ri *reqInfo, body []byte) {
	if b.opts.CLI == nil {
		writeErr(w, r.URL.Path, http.StatusNotFound, "this broker serves no fleet agents")
		return
	}
	r.Header.Set("Authorization", "Bearer "+b.opts.CLI.Token())
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	if ri.rec.Backend == "" {
		b.opts.CLI.ServeHTTP(w, r)
		return
	}
	cw := newCaptureWriter(w)
	b.opts.CLI.ServeHTTP(cw, r)
	ri.rec.Routed = cw.Header().Get("X-Bashy-Routed")
	ri.rec.PromptTok, ri.rec.OutputTok = parseTokens(cw.tail)
	b.finish(ri, cw.status, 0, "")
}

// ---- local inventory -------------------------------------------------------

// canonicalModel adds the implicit :latest tag.
func canonicalModel(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	last := name[strings.LastIndex(name, "/")+1:]
	if !strings.Contains(last, ":") {
		return name + ":latest"
	}
	return name
}

func (b *Broker) refreshTags(ctx context.Context, force bool) map[string]engineModel {
	b.tagMu.Lock()
	defer b.tagMu.Unlock()
	if !force && b.tags != nil && time.Since(b.tagsAt) < 5*time.Second {
		return b.tags
	}
	models, err := b.engine.tags(ctx)
	if err != nil {
		if b.tags == nil {
			return map[string]engineModel{}
		}
		return b.tags
	}
	m := make(map[string]engineModel, len(models))
	for _, x := range models {
		m[canonicalModel(x.Name)] = x
	}
	b.tags, b.tagsAt = m, time.Now()
	return m
}

// localModel reports whether model is on the engine, by canonical name.
func (b *Broker) localModel(ctx context.Context, model string) (engineModel, bool) {
	if !b.local || model == "" {
		return engineModel{}, false
	}
	key := canonicalModel(model)
	if m, ok := b.refreshTags(ctx, false)[key]; ok {
		return m, true
	}
	m, ok := b.refreshTags(ctx, true)[key]
	return m, ok
}

// derivedModel returns a model that shares base's weights and carries
// params as defaults, creating it once. Name: <base>-bashy-<hash8>.
func (b *Broker) derivedModel(ctx context.Context, base string, params map[string]any) (string, error) {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%v;", k, params[k])
	}
	name := base + "-bashy-" + hex.EncodeToString(h.Sum(nil))[:8]
	b.tagMu.Lock()
	known := b.derived[name]
	b.tagMu.Unlock()
	if known {
		return name, nil
	}
	if _, ok := b.localModel(ctx, name); !ok {
		if err := b.engine.create(ctx, name, base, params); err != nil {
			return "", fmt.Errorf("derive %s with %v: %w", base, params, err)
		}
	}
	b.tagMu.Lock()
	b.derived[name] = true
	b.tagMu.Unlock()
	return name, nil
}

// isDerived hides the broker's derived models from listings.
func isDerived(name string) bool { return strings.Contains(name, "-bashy-") }

// ---- sticky ----------------------------------------------------------------

// stickyFor finds or implicitly creates the binding a request names.
func (b *Broker) stickyFor(ctx context.Context, r *http.Request, ri *reqInfo, payload map[string]json.RawMessage, model string) (*Binding, *StickySpec, error) {
	var spec *StickySpec
	if h := strings.TrimSpace(r.Header.Get(StickyHeader)); h != "" {
		s, err := ParseStickyHeader(h)
		if err != nil {
			return nil, nil, stickyErr(400, "%v", err)
		}
		spec = &s
	}
	if raw, ok := payload["bashy"]; ok {
		var ext struct {
			Sticky *StickySpec `json:"sticky"`
		}
		if err := json.Unmarshal(raw, &ext); err != nil {
			return nil, nil, stickyErr(400, "sticky: bashy extension: %v", err)
		}
		delete(payload, "bashy") // never forwarded to a backend
		if ext.Sticky != nil && spec == nil {
			if err := ext.Sticky.validate(); err != nil {
				return nil, nil, stickyErr(400, "%v", err)
			}
			spec = ext.Sticky
		}
	}
	key := ri.params["sticky"]
	if key == "" && spec != nil {
		key = spec.Key
	}
	if key == "" {
		return nil, nil, nil
	}
	if bnd := b.sticky.get(ri.principal, ri.session, key); bnd != nil {
		return bnd, spec, nil
	}
	if spec == nil {
		return nil, nil, stickyErr(404, "sticky: no binding %q in this scope; create it (bashy llm sticky create %s --model M) or send %s with a model", key, key, StickyHeader)
	}
	spec.Key = key
	bnd, err := b.createBinding(ctx, ri.principal, ri.session, *spec, model)
	return bnd, spec, err
}

// createBinding resolves a spec to an identity and stores it.
func (b *Broker) createBinding(ctx context.Context, principal, session string, spec StickySpec, reqModel string) (*Binding, error) {
	if err := spec.validate(); err != nil {
		return nil, stickyErr(400, "%v", err)
	}
	if name := slashOf(spec.Filter); name != "" {
		return nil, stickyErr(400, "sticky: filter slash=%s runs a tool command and cannot be frozen into a binding (identities serve completions only)", name)
	}
	id, err := b.resolveIdentity(ctx, principal, session, spec, reqModel)
	if err != nil {
		return nil, err
	}
	digest := id.Digest()
	if spec.Identity != "" && !strings.HasPrefix(ShortDigest(digest), ShortDigest(spec.Identity)) {
		return nil, stickyErr(409, "sticky: identity %s is not what %q resolves to now (%s)", ShortDigest(spec.Identity), firstNonEmpty(spec.Model, reqModel), ShortDigest(digest))
	}
	bnd := &Binding{
		scoped:   scoped{Principal: principal, Session: session, Exported: spec.Export, Created: b.opts.Now()},
		Spec:     spec,
		Identity: id,
		Digest:   digest,
	}
	return b.sticky.put(bnd)
}

func (b *Broker) resolveIdentity(ctx context.Context, principal, session string, spec StickySpec, reqModel string) (Identity, error) {
	model := firstNonEmpty(spec.Model, reqModel)
	if model == "" && spec.Identity != "" {
		if other := b.sticky.findDigest(principal, session, spec.Identity); other != nil {
			return other.Identity, nil
		}
		return Identity{}, stickyErr(409, "sticky: no binding with identity %s is visible here; create one with a model first", ShortDigest(spec.Identity))
	}
	if model == "" {
		return Identity{}, stickyErr(400, "sticky: a model is required to create binding %q", spec.Key)
	}
	options := map[string]any{}
	for k, v := range spec.Options {
		options[k] = v
	}
	if m, ok := b.localModel(ctx, model); ok {
		if _, set := options["num_ctx"]; !set && b.opts.EngineContext > 0 {
			options["num_ctx"] = float64(b.opts.EngineContext)
		}
		return Identity{Backend: BackendLocal, Location: "local", Model: canonicalModel(m.Name), ModelDigest: m.Digest, Options: nilIfEmpty(options)}, nil
	}
	if b.opts.CLI == nil {
		return Identity{}, stickyErr(404, "sticky: model %q is not local and this broker serves no fleet agents", model)
	}
	info, err := b.opts.CLI.ResolveAgent(ctx, model, spec.Filter)
	if err != nil {
		return Identity{}, stickyErr(404, "sticky: %v", err)
	}
	version := ""
	if b.opts.ToolVersion != nil {
		version = b.opts.ToolVersion(info.Tool)
	}
	return Identity{
		Backend: BackendCLI, Location: "local", Model: info.Model, Agent: info.Name,
		Tool: info.Tool, ToolVersion: version, VendorModel: info.VendorModel, Provider: info.Provider,
		Launch: "cligw-pure-completion/" + firstNonEmpty(info.Warm, "cold"), Effort: info.Effort, Options: nilIfEmpty(options),
	}, nil
}

// checkIdentity refuses when the frozen identity can no longer be served
// exactly (model re-pulled with a new digest, CLI upgraded).
func (b *Broker) checkIdentity(ctx context.Context, bnd *Binding) error {
	id := bnd.Identity
	switch id.Backend {
	case BackendLocal:
		m, ok := b.localModel(ctx, id.Model)
		if !ok {
			return stickyErr(503, "sticky: identity %s: model %s is no longer on the engine", ShortDigest(bnd.Digest), id.Model)
		}
		if id.ModelDigest != "" && m.Digest != id.ModelDigest {
			return stickyErr(409, "sticky: identity %s: model %s changed (digest %s, was %s); the binding cannot be served exactly", ShortDigest(bnd.Digest), id.Model, ShortDigest(m.Digest), ShortDigest(id.ModelDigest))
		}
	case BackendCLI:
		if b.opts.ToolVersion != nil && id.ToolVersion != "" {
			if v := b.opts.ToolVersion(id.Tool); v != id.ToolVersion {
				return stickyErr(409, "sticky: identity %s: %s is now %q (was %q); the binding cannot be served exactly", ShortDigest(bnd.Digest), id.Tool, v, id.ToolVersion)
			}
		}
		if lk, ok := b.opts.CLI.(AgentLookup); ok && id.Agent != "" {
			if cur, found := lk.LookupAgent(id.Agent); found && cur.Effort != id.Effort {
				return stickyErr(409, "sticky: identity %s: agent %s now declares effort %q (was %q); the binding cannot be served exactly", ShortDigest(bnd.Digest), id.Agent, cur.Effort, id.Effort)
			}
		}
	}
	return nil
}

func nilIfEmpty(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// serveStickyAPI: POST /v1/sticky (create), GET /v1/sticky (list),
// GET|DELETE /v1/sticky/<key>.
func (b *Broker) serveStickyAPI(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/sticky"), "/")
	switch {
	case key == "" && r.Method == http.MethodPost:
		body, err := readBody(r)
		if err != nil {
			writeErr(w, r.URL.Path, http.StatusBadRequest, err.Error())
			return
		}
		var spec StickySpec
		if err := json.Unmarshal(body, &spec); err != nil {
			writeErr(w, r.URL.Path, http.StatusBadRequest, "sticky: "+err.Error())
			return
		}
		bnd, err := b.createBinding(r.Context(), ri.principal, ri.session, spec, "")
		if err != nil {
			writeStickyErr(w, r.URL.Path, err)
			return
		}
		writeJSON(w, http.StatusOK, stickyView(bnd))
	case key == "" && r.Method == http.MethodGet:
		list := b.sticky.list(ri.principal, ri.session)
		out := make([]map[string]any, 0, len(list))
		for i := range list {
			out = append(out, stickyView(&list[i]))
		}
		writeJSON(w, http.StatusOK, map[string]any{"schema_version": "bashy-sticky-list-v1", "bindings": out})
	case key != "" && r.Method == http.MethodGet:
		bnd := b.sticky.get(ri.principal, ri.session, key)
		if bnd == nil {
			writeErr(w, r.URL.Path, http.StatusNotFound, "sticky: no binding "+key+" in this scope")
			return
		}
		writeJSON(w, http.StatusOK, stickyView(bnd))
	case key != "" && r.Method == http.MethodDelete:
		if !b.sticky.delete(ri.principal, ri.session, key) {
			writeErr(w, r.URL.Path, http.StatusNotFound, "sticky: no binding "+key+" in this scope")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": key})
	default:
		writeErr(w, r.URL.Path, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func stickyView(b *Binding) map[string]any {
	return map[string]any{
		"schema_version": "bashy-sticky-v1",
		"key":            b.Spec.Key,
		"spec":           b.Spec,
		"identity":       b.Identity,
		"digest":         b.Digest,
		"short":          ShortDigest(b.Digest),
		"used":           b.Used,
		"remaining":      b.Remaining(),
		"session":        b.Session,
		"exported":       b.Exported,
		"created":        b.Created,
		"last_used":      b.LastUsed,
	}
}

func (b *Broker) serveTurns(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	id, tail, _ := strings.Cut(rest, "/")
	if tail != "turns" || r.Method != http.MethodGet {
		writeErr(w, r.URL.Path, http.StatusNotFound, "want GET /v1/sessions/<id>/turns")
		return
	}
	if v, err := url.PathUnescape(id); err == nil {
		id = v
	}
	if err := ValidateSession(id); err != nil || id == "" {
		writeErr(w, r.URL.Path, http.StatusBadRequest, "invalid session id")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": "bashy-session-turns-v1", "session": id, "turns": b.audit.turns(ri.principal, id)})
}

// ---- models and health -----------------------------------------------------

func (b *Broker) serveModels(w http.ResponseWriter, r *http.Request, ri *reqInfo) {
	var data []json.RawMessage
	// A slash= listing asks for agents that declare a tool command; local
	// engine models declare none.
	slashListing := r.URL.Query().Get("slash") != "" || slashOf(r.Header.Get(cligw.FilterHeader)) != ""
	if b.local && !slashListing {
		tags := b.refreshTags(r.Context(), false)
		names := make([]string, 0, len(tags))
		for n := range tags {
			if !isDerived(n) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			m := tags[n]
			entry, _ := json.Marshal(map[string]any{
				"id": m.Name, "object": "model", "owned_by": "local",
				"x_backend": BackendLocal, "x_locality": "local", "x_kind": "local",
				"x_digest": m.Digest, "x_size": m.Size,
			})
			data = append(data, entry)
		}
	}
	if b.opts.CLI != nil {
		req := httptestRequest(r.Context(), http.MethodGet, "/v1/models"+queryOf(r))
		req.Header.Set("Authorization", "Bearer "+b.opts.CLI.Token())
		if f := r.Header.Get(cligw.FilterHeader); f != "" {
			req.Header.Set(cligw.FilterHeader, f)
		}
		rec := &bufferWriter{header: http.Header{}}
		b.opts.CLI.ServeHTTP(rec, req)
		if rec.status >= http.StatusBadRequest {
			// A bad filter (unknown slash name, malformed key) is the
			// caller's to see, not an empty listing.
			for k, v := range rec.header {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.status)
			_, _ = w.Write(rec.buf.Bytes())
			return
		}
		var list struct {
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(rec.buf.Bytes(), &list) == nil {
			data = append(data, list.Data...)
		}
	}
	if data == nil {
		data = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// slashOf returns the slash= key of an X-Bashy-Filter value ("" when absent
// or unparsable — a malformed filter is cligw's 400 to report).
func slashOf(filter string) string {
	f, err := cligw.ParseFilter(filter)
	if err != nil {
		return ""
	}
	return f.Slash
}

func queryOf(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

// Health is the bashy-broker-health-v1 envelope.
type Health struct {
	SchemaVersion string          `json:"schema_version"`
	PID           int             `json:"pid"`
	Local         bool            `json:"local_engine"`
	EngineUp      bool            `json:"engine_up"`
	Device        DeviceStats     `json:"device"`
	Resident      []string        `json:"resident_models,omitempty"`
	Sticky        int             `json:"sticky_bindings"`
	CLI           json.RawMessage `json:"cligw,omitempty"`
}

func (b *Broker) serveHealth(w http.ResponseWriter, r *http.Request) {
	h := Health{SchemaVersion: "bashy-broker-health-v1", PID: os.Getpid(), Local: b.local, Device: b.device.Stats()}
	if b.local {
		h.EngineUp = engineUp(b.engine.base)
		if ps, err := b.engine.ps(r.Context()); err == nil {
			for _, m := range ps {
				h.Resident = append(h.Resident, m.Name)
			}
		}
	}
	b.sticky.mu.Lock()
	h.Sticky = len(b.sticky.m)
	b.sticky.mu.Unlock()
	if b.opts.CLI != nil {
		req := httptestRequest(r.Context(), http.MethodGet, "/health")
		req.Header.Set("Authorization", "Bearer "+b.opts.CLI.Token())
		rec := &bufferWriter{header: http.Header{}}
		b.opts.CLI.ServeHTTP(rec, req)
		if json.Valid(rec.buf.Bytes()) {
			h.CLI = rec.buf.Bytes()
		}
	}
	writeJSON(w, http.StatusOK, h)
}

// ---- serving ---------------------------------------------------------------

// Serve runs the door on the given listeners until ctx ends. A unix
// listener authenticates kernel peer credentials in addition to its 0600 mode.
func (b *Broker) Serve(ctx context.Context, listeners ...net.Listener) error {
	srv := &http.Server{
		Handler:           b,
		ReadHeaderTimeout: 30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if c.LocalAddr().Network() == "unix" {
				return context.WithValue(ctx, unixOwnerKey, socketOwner(c))
			}
			return ctx
		},
	}
	errs := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func(ln net.Listener) { errs <- srv.Serve(ln) }(ln)
	}
	select {
	case err := <-errs:
		_ = srv.Close()
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

// ---- helpers ---------------------------------------------------------------

const maxBody = 32 << 20

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxBody {
		return nil, errors.New("request body too large")
	}
	return data, nil
}

func jsonString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers in the dialect of the surface: Ollama's {"error": "..."}
// under /api/, OpenAI's envelope elsewhere.
func writeErr(w http.ResponseWriter, path string, status int, msg string) {
	if strings.HasPrefix(path, "/api/") {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "bashy_broker", "code": status}})
}

func writeStickyErr(w http.ResponseWriter, path string, err error) {
	var se *StickyError
	if errors.As(err, &se) {
		if se.Status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		writeErr(w, path, se.Status, se.Msg)
		return
	}
	writeErr(w, path, http.StatusInternalServerError, err.Error())
}

func httptestRequest(ctx context.Context, method, target string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, method, "http://broker"+target, http.NoBody)
	return req
}

// bufferWriter is a minimal in-memory ResponseWriter for sub-requests.
type bufferWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferWriter) Header() http.Header         { return b.header }
func (b *bufferWriter) WriteHeader(code int)        { b.status = code }
func (b *bufferWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }
