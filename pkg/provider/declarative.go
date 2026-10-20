package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The built-in engine for declarative provider manifests (S472.13a item 2;
// S450.1 amendment A4). A declarative package is data: the endpoints of
// one HTTP API, request templates, where the credential goes, and how
// responses map onto the normalized Resource. The engine renders the
// templates by pure substitution — ${field} placeholders replaced with
// request values, nothing evaluated, no functions, no shell — calls the
// declared endpoints with net/http, and maps the answers. It never starts
// a process, so the master can run it with exec disabled.
//
// The engine keeps the same discipline as the DigitalOcean reference
// adapter: every resource it creates carries the bashy-lease tags
// (LeaseTags), a replayed create finds its resource by the idempotency tag
// instead of creating another, and an existing resource is acted on only
// when it carries this owner's tags. Those tags are the only state.

// DeclarativeDoc is the declarative document a Manifest.Declarative
// decodes into.
type DeclarativeDoc struct {
	// BaseURL is the API root every operation path is joined to.
	BaseURL string `json:"base_url"`
	// Headers are sent on every request (templated; the credential is
	// not allowed here — see Auth).
	Headers map[string]string `json:"headers,omitempty"`
	// Auth places the credential.
	Auth DeclarativeAuth `json:"auth"`
	// Resource maps a provider resource object onto the normalized
	// Resource (JSON paths relative to the operation's Result).
	Resource DeclarativeResource `json:"resource"`
	// States maps the provider's raw status strings onto normalized
	// states; anything unmapped is StateUnknown.
	States map[string]State `json:"states"`
	// Operations declares one endpoint per supported Op. The manifest's
	// capability list must match its keys (plan needs no endpoint).
	Operations map[Op]*DeclarativeEndpoint `json:"operations"`
}

// DeclarativeAuth says where the credential goes on each request.
type DeclarativeAuth struct {
	// In is "header" or "query".
	In string `json:"in"`
	// Name is the header or query parameter.
	Name string `json:"name"`
	// Value is a template that must use ${credential} (default:
	// "${credential}"; e.g. "Bearer ${credential}").
	Value string `json:"value,omitempty"`
}

// DeclarativeResource maps a resource object's JSON paths. ID, Status and
// Tags are required; the others are optional. Paths are dot-separated
// keys (array indexes are digits), no functions.
type DeclarativeResource struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status"`
	Region    string `json:"region,omitempty"`
	Size      string `json:"size,omitempty"`
	Tags      string `json:"tags"`
	PublicIPs string `json:"public_ips,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	// Retained lists what the provider keeps (and bills) after the
	// resource is destroyed or stopped, e.g. attached volumes.
	Retained string `json:"retained,omitempty"`
}

// DeclarativeEndpoint is one operation's HTTP call.
type DeclarativeEndpoint struct {
	Method string `json:"method"`
	// Path is a template joined to BaseURL.
	Path string `json:"path"`
	// Query holds templated query parameters.
	Query map[string]string `json:"query,omitempty"`
	// Body is a JSON template: every string value is expanded, and a
	// string that is exactly "${tags}" becomes the JSON array of tags.
	Body json.RawMessage `json:"body,omitempty"`
	// Result is the JSON path of the resource object (or, for list, the
	// array of them) in the response. Empty means the whole body; for
	// operations whose response carries no resource (a 204 destroy, an
	// action receipt) set NoResource.
	Result string `json:"result,omitempty"`
	// NoResource says the response does not describe the resource; the
	// engine inspects it afterwards (or, for destroy, reports it deleted).
	NoResource bool `json:"no_resource,omitempty"`
}

// Placeholders accepted in templates. ${credential} is accepted only in
// Auth.Value, so a document cannot route the credential into a body, a
// path or a query parameter by another name.
const (
	PhCredential     = "credential"
	PhResourceID     = "resource_id"
	PhIdempotencyKey = "idempotency_key"
	PhIdempotencyTag = "idempotency_tag"
	PhOwnerTag       = "owner_tag"
	PhName           = "name"
	PhTags           = "tags" // JSON array in a body; comma-joined elsewhere
	PhSpecName       = "spec.name"
	PhSpecRegion     = "spec.region"
	PhSpecSize       = "spec.size"
	PhSpecImage      = "spec.image"
	PhSpecUserData   = "spec.user_data"
)

var placeholder = regexp.MustCompile(`\$\{([A-Za-z0-9_.]+)\}`)

// Expand substitutes ${field} placeholders in tmpl from vars. An unknown
// placeholder is an error: templates are closed, nothing is evaluated.
func Expand(tmpl string, vars map[string]string) (string, error) {
	var missing []string
	out := placeholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := m[2 : len(m)-1]
		v, ok := vars[key]
		if !ok {
			missing = append(missing, key)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unknown placeholder ${%s}", missing[0])
	}
	return out, nil
}

// DeclarativeConfig configures the engine for one provider.
type DeclarativeConfig struct {
	// Credential is held in memory only; it is placed as Auth declares
	// and never written anywhere.
	Credential string
	// Owner scopes ownership (ValidOwner).
	Owner string
	// HTTPClient defaults to a client with a 60s timeout.
	HTTPClient *http.Client
}

// ParseDeclarative decodes and validates m.Declarative.
func ParseDeclarative(m Manifest) (*DeclarativeDoc, error) {
	bad := func(format string, args ...any) error {
		return &Error{Code: ErrInvalidRequest, Message: "manifest " + m.Name + ": declarative: " + fmt.Sprintf(format, args...)}
	}
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}
	if m.Form != FormDeclarative {
		return nil, bad("form %q is not declarative", m.Form)
	}
	var doc DeclarativeDoc
	dec := json.NewDecoder(bytes.NewReader(m.Declarative))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, bad("%v", err)
	}
	u, err := url.Parse(doc.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, bad("invalid base_url %q", doc.BaseURL)
	}
	doc.BaseURL = strings.TrimRight(doc.BaseURL, "/")
	switch doc.Auth.In {
	case "header", "query":
	default:
		return nil, bad("auth.in must be header or query, got %q", doc.Auth.In)
	}
	if doc.Auth.Name == "" {
		return nil, bad("auth.name is required")
	}
	if doc.Auth.Value == "" {
		doc.Auth.Value = "${" + PhCredential + "}"
	}
	if !strings.Contains(doc.Auth.Value, "${"+PhCredential+"}") {
		return nil, bad("auth.value must place ${credential}")
	}
	if doc.Resource.ID == "" || doc.Resource.Status == "" || doc.Resource.Tags == "" {
		return nil, bad("resource.id, resource.status and resource.tags are required")
	}
	for k, v := range doc.Headers {
		if strings.Contains(k+v, "${"+PhCredential+"}") {
			return nil, bad("headers may not place ${credential}; use auth")
		}
	}
	for op, ep := range doc.Operations {
		if ep == nil {
			return nil, bad("operation %s is null", op)
		}
		if !op.Known() {
			return nil, bad("unknown operation %q", op)
		}
		if op == OpPlan {
			return nil, bad("plan is rendered locally and takes no endpoint")
		}
		switch strings.ToUpper(ep.Method) {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			ep.Method = strings.ToUpper(ep.Method)
		default:
			return nil, bad("operation %s: unsupported method %q", op, ep.Method)
		}
		if ep.Path == "" || !strings.HasPrefix(ep.Path, "/") {
			return nil, bad("operation %s: path must start with /", op)
		}
		for _, t := range append([]string{ep.Path, ep.Result}, mapValues(ep.Query)...) {
			if strings.Contains(t, "${"+PhCredential+"}") {
				return nil, bad("operation %s may not place ${credential}; use auth", op)
			}
		}
		if len(ep.Body) > 0 {
			var v any
			if err := json.Unmarshal(ep.Body, &v); err != nil {
				return nil, bad("operation %s: body is not JSON: %v", op, err)
			}
			if strings.Contains(string(ep.Body), "${"+PhCredential+"}") {
				return nil, bad("operation %s: body may not place ${credential}; use auth", op)
			}
		}
	}
	for _, c := range m.Capabilities {
		op := Op(c)
		if !op.Known() || op == OpPlan {
			continue
		}
		if doc.Operations[op] == nil {
			return nil, bad("capability %q has no operation endpoint", c)
		}
	}
	if m.Supports(OpCreate) && !m.Supports(OpList) {
		return nil, bad("create needs list (idempotent create looks the resource up by its tag)")
	}
	return &doc, nil
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// NewDeclarativeAdapter returns the engine for a declarative manifest,
// wrapped in Guard. It makes no network call until Do.
func NewDeclarativeAdapter(m Manifest, cfg DeclarativeConfig) (Adapter, error) {
	doc, err := ParseDeclarative(m)
	if err != nil {
		return nil, err
	}
	if cfg.Credential == "" {
		return nil, &Error{Code: ErrInvalidCredentials, Message: "manifest " + m.Name + ": credential is required"}
	}
	if !ValidOwner(cfg.Owner) {
		return nil, &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("manifest %s: owner %q must match [A-Za-z0-9_-]{1,64}", m.Name, cfg.Owner)}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return Guard(&declarative{m: m, doc: doc, cfg: cfg}), nil
}

type declarative struct {
	m   Manifest
	doc *DeclarativeDoc
	cfg DeclarativeConfig
}

func (d *declarative) Manifest() Manifest { return d.m }

func (d *declarative) Do(ctx context.Context, req Request) (Result, error) {
	switch req.Op {
	case OpValidate, OpCapacity:
		var body any
		if err := d.call(ctx, req, nil, &body); err != nil {
			return d.fail(req.Op, err)
		}
		data, _ := json.Marshal(body)
		return d.ok(req.Op, &Result{Data: data})
	case OpPlan:
		return d.plan(req)
	case OpCreate:
		return d.create(ctx, req)
	case OpInspect:
		r, _, res, err := d.owned(ctx, req)
		if err != nil {
			return res, err
		}
		return d.ok(req.Op, &Result{Resource: r})
	case OpList:
		rs, err := d.list(ctx)
		if err != nil {
			return d.fail(req.Op, err)
		}
		out := []Resource{}
		for _, r := range rs {
			if Owned(r.Tags, d.cfg.Owner) {
				out = append(out, r)
			}
		}
		return d.ok(req.Op, &Result{Resources: out})
	case OpStart:
		return d.action(ctx, req, StateRunning)
	case OpStop:
		return d.action(ctx, req, StateStopped)
	case OpReboot, OpResize:
		return d.action(ctx, req, "")
	case OpDestroy:
		return d.destroy(ctx, req)
	}
	return Failure(req.Op, ErrUnsupportedCapability, "%s: operation %q", d.m.Name, req.Op)
}

// ---- rendering ----

func resourceName(req Request) string {
	if req.Spec != nil && req.Spec.Name != "" {
		return req.Spec.Name
	}
	return "bashy-" + strings.TrimPrefix(IdempotencyTag(req.IdempotencyKey), tagIdemPrefix)[:12]
}

func (d *declarative) createTags(req Request) []string {
	tags := LeaseTags(d.cfg.Owner, req.IdempotencyKey, req.Authorization)
	if req.Spec != nil {
		for _, t := range req.Spec.Tags {
			if !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// vars builds the substitution set for req. The credential is NOT in it;
// it is added only when Auth.Value is rendered.
func (d *declarative) vars(req Request) (map[string]string, []string) {
	tags := d.createTags(req)
	v := map[string]string{
		PhResourceID:     req.ResourceID,
		PhIdempotencyKey: req.IdempotencyKey,
		PhIdempotencyTag: IdempotencyTag(req.IdempotencyKey),
		PhOwnerTag:       OwnerTag(d.cfg.Owner),
		PhName:           resourceName(req),
		PhTags:           strings.Join(tags, ","),
		PhSpecName:       "", PhSpecRegion: "", PhSpecSize: "", PhSpecImage: "", PhSpecUserData: "",
	}
	if s := req.Spec; s != nil {
		v[PhSpecName], v[PhSpecRegion], v[PhSpecSize], v[PhSpecImage] = s.Name, s.Region, s.Size, s.Image
		v[PhSpecUserData] = string(s.UserData) // opaque, passed through unread
	}
	return v, tags
}

// rendered is one fully rendered request, credential excluded.
type rendered struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

func (d *declarative) render(req Request, ep *DeclarativeEndpoint) (*rendered, error) {
	vars, tags := d.vars(req)
	path, err := Expand(ep.Path, vars)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(d.doc.BaseURL + path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	keys := make([]string, 0, len(ep.Query))
	for k := range ep.Query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, err := Expand(ep.Query[k], vars)
		if err != nil {
			return nil, err
		}
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	out := &rendered{Method: ep.Method, URL: u.String(), Headers: map[string]string{}}
	for k, t := range d.doc.Headers {
		v, err := Expand(t, vars)
		if err != nil {
			return nil, err
		}
		out.Headers[k] = v
	}
	if len(ep.Body) > 0 {
		var tree any
		if err := json.Unmarshal(ep.Body, &tree); err != nil {
			return nil, err
		}
		tree, err = expandTree(tree, vars, tags)
		if err != nil {
			return nil, err
		}
		out.Body, err = json.Marshal(tree)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// expandTree substitutes every string in a decoded JSON template. A
// string that is exactly "${tags}" becomes the tag array.
func expandTree(v any, vars map[string]string, tags []string) (any, error) {
	switch t := v.(type) {
	case string:
		if t == "${"+PhTags+"}" {
			return tags, nil
		}
		return Expand(t, vars)
	case []any:
		for i := range t {
			x, err := expandTree(t[i], vars, tags)
			if err != nil {
				return nil, err
			}
			t[i] = x
		}
		return t, nil
	case map[string]any:
		for k := range t {
			x, err := expandTree(t[k], vars, tags)
			if err != nil {
				return nil, err
			}
			t[k] = x
		}
		return t, nil
	}
	return v, nil
}

func (d *declarative) plan(req Request) (Result, error) {
	ep := d.doc.Operations[OpCreate]
	if ep == nil {
		return Failure(req.Op, ErrUnsupportedCapability, "%s: no create operation to plan", d.m.Name)
	}
	r, err := d.render(req, ep)
	if err != nil {
		return Failure(req.Op, ErrInvalidRequest, "%s: %v", d.m.Name, err)
	}
	data, _ := json.Marshal(r)
	return d.ok(req.Op, &Result{Data: data})
}

// ---- HTTP ----

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	b := e.body
	if len(b) > 200 {
		b = b[:200] + "…"
	}
	return fmt.Sprintf("http %d: %s", e.status, strings.TrimSpace(b))
}

type transportErr struct{ err error }

func (e *transportErr) Error() string { return e.err.Error() }
func (e *transportErr) Unwrap() error { return e.err }

// call renders the endpoint for req.Op (or ep when given) and performs it.
// A 204 or empty body leaves out untouched.
func (d *declarative) call(ctx context.Context, req Request, ep *DeclarativeEndpoint, out *any) error {
	if ep == nil {
		ep = d.doc.Operations[req.Op]
	}
	if ep == nil {
		return &Error{Code: ErrUnsupportedCapability, Message: fmt.Sprintf("%s: no endpoint for %s", d.m.Name, req.Op)}
	}
	r, err := d.render(req, ep)
	if err != nil {
		return &Error{Code: ErrInvalidRequest, Message: d.m.Name + ": " + err.Error()}
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	hreq, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return &Error{Code: ErrInvalidRequest, Message: d.m.Name + ": " + err.Error()}
	}
	for k, v := range r.Headers {
		hreq.Header.Set(k, v)
	}
	if body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	hreq.Header.Set("Accept", "application/json")
	// The credential exists only here, on the wire.
	authValue, _ := Expand(d.doc.Auth.Value, map[string]string{PhCredential: d.cfg.Credential})
	switch d.doc.Auth.In {
	case "header":
		hreq.Header.Set(d.doc.Auth.Name, authValue)
	case "query":
		q := hreq.URL.Query()
		q.Set(d.doc.Auth.Name, authValue)
		hreq.URL.RawQuery = q.Encode()
	}
	resp, err := d.cfg.HTTPClient.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return &Error{Code: ErrTimeout, Message: d.m.Name + ": " + ctx.Err().Error(), Retryable: true}
		}
		return &transportErr{err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResultBytes))
	if err != nil {
		if ctx.Err() != nil {
			return &Error{Code: ErrTimeout, Message: d.m.Name + ": " + ctx.Err().Error(), Retryable: true}
		}
		return &transportErr{err}
	}
	if resp.StatusCode >= 400 {
		return &httpError{status: resp.StatusCode, body: string(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{Code: ErrProvider, Message: fmt.Sprintf("%s: response is not JSON: %v", d.m.Name, err)}
		}
	}
	return nil
}

// maybeApplied reports whether err leaves open the possibility that the
// provider performed the request.
func declMaybeApplied(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.status >= 500
	}
	var te *transportErr
	var pe *Error
	return errors.As(err, &te) || (errors.As(err, &pe) && pe.Code == ErrTimeout)
}

func (d *declarative) normalize(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	var he *httpError
	if errors.As(err, &he) {
		e := &Error{Message: d.m.Name + ": " + he.Error()}
		switch {
		case he.status == http.StatusUnauthorized || he.status == http.StatusForbidden:
			e.Code = ErrInvalidCredentials
		case he.status == http.StatusNotFound:
			e.Code = ErrNotFound
		case he.status == http.StatusTooManyRequests:
			e.Code, e.Retryable = ErrRateLimited, true
		case he.status >= 500:
			e.Code, e.Retryable = ErrProvider, true
		default:
			e.Code = ErrInvalidRequest
		}
		return e
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &ne) && ne.Timeout()) {
		return &Error{Code: ErrTimeout, Message: d.m.Name + ": " + err.Error(), Retryable: true}
	}
	return &Error{Code: ErrProvider, Message: d.m.Name + ": " + err.Error(), Retryable: true}
}

func (d *declarative) fail(op Op, err error) (Result, error) {
	e := d.normalize(err)
	return Result{ProtocolVersion: ProtocolVersion, Op: op, Error: e}, e
}

func (d *declarative) ok(op Op, r *Result) (Result, error) {
	r.ProtocolVersion = ProtocolVersion
	r.Op = op
	return *r, nil
}

// ---- response mapping ----

// lookup walks a dot path through decoded JSON.
func lookup(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	for _, seg := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			x, ok := t[seg]
			if !ok {
				return nil, false
			}
			v = x
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return fmt.Sprint(v)
}

func lookupString(v any, path string) string {
	if path == "" {
		return ""
	}
	x, ok := lookup(v, path)
	if !ok {
		return ""
	}
	return scalar(x)
}

func lookupStrings(v any, path string) []string {
	if path == "" {
		return nil
	}
	x, ok := lookup(v, path)
	if !ok {
		return nil
	}
	switch t := x.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := scalar(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return strings.Split(t, ",")
	}
	return nil
}

func (d *declarative) state(raw string) State {
	if s, ok := d.doc.States[raw]; ok {
		return s
	}
	return StateUnknown
}

// resource maps one provider resource object.
func (d *declarative) resource(obj any) *Resource {
	m := d.doc.Resource
	r := &Resource{
		ID:        lookupString(obj, m.ID),
		Name:      lookupString(obj, m.Name),
		RawStatus: lookupString(obj, m.Status),
		Region:    lookupString(obj, m.Region),
		Size:      lookupString(obj, m.Size),
		Tags:      lookupStrings(obj, m.Tags),
		PublicIPs: lookupStrings(obj, m.PublicIPs),
	}
	r.State = d.state(r.RawStatus)
	if ts := lookupString(obj, m.CreatedAt); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			r.CreatedAt = t
		}
	}
	return r
}

func (d *declarative) retained(obj any) []string {
	return lookupStrings(obj, d.doc.Resource.Retained)
}

// extract pulls the operation's result object out of a response body.
func (d *declarative) extract(ep *DeclarativeEndpoint, body any) (any, error) {
	obj, ok := lookup(body, ep.Result)
	if !ok {
		return nil, &Error{Code: ErrProvider, Message: fmt.Sprintf("%s: response has no %q", d.m.Name, ep.Result)}
	}
	return obj, nil
}

// ---- operations ----

func (d *declarative) list(ctx context.Context) ([]Resource, error) {
	ep := d.doc.Operations[OpList]
	req := Request{Op: OpList}
	var body any
	if err := d.call(ctx, req, ep, &body); err != nil {
		return nil, err
	}
	obj, err := d.extract(ep, body)
	if err != nil {
		return nil, err
	}
	items, ok := obj.([]any)
	if !ok {
		return nil, &Error{Code: ErrProvider, Message: fmt.Sprintf("%s: list result %q is not an array", d.m.Name, ep.Result)}
	}
	out := make([]Resource, 0, len(items))
	for _, it := range items {
		out = append(out, *d.resource(it))
	}
	return out, nil
}

// findByKey returns this owner's resource created under key, if any.
func (d *declarative) findByKey(ctx context.Context, key string) (*Resource, error) {
	rs, err := d.list(ctx)
	if err != nil {
		return nil, err
	}
	tag := IdempotencyTag(key)
	var mine []Resource
	for _, r := range rs {
		if Owned(r.Tags, d.cfg.Owner) && slices.Contains(r.Tags, tag) {
			mine = append(mine, r)
		}
	}
	switch len(mine) {
	case 0:
		return nil, nil
	case 1:
		return &mine[0], nil
	}
	ids := make([]string, len(mine))
	for i, r := range mine {
		ids[i] = r.ID
	}
	return nil, &Error{Code: ErrNeedsManualReconcile, Message: fmt.Sprintf("%s: idempotency key matches %d resources (%s)", d.m.Name, len(mine), strings.Join(ids, ", "))}
}

// create is idempotent on the key, exactly like the reference adapter: an
// existing resource tagged with the key is returned; a create whose
// failure may have taken effect is looked up again before it is reported.
func (d *declarative) create(ctx context.Context, req Request) (Result, error) {
	if r, err := d.findByKey(ctx, req.IdempotencyKey); err != nil {
		return d.fail(req.Op, err)
	} else if r != nil {
		return d.ok(req.Op, &Result{Resource: r})
	}
	ep := d.doc.Operations[OpCreate]
	var body any
	err := d.call(ctx, req, ep, &body)
	if err == nil {
		if ep.NoResource {
			if r, ferr := d.findByKey(ctx, req.IdempotencyKey); ferr == nil && r != nil {
				return d.ok(req.Op, &Result{Resource: r})
			} else if ferr != nil {
				return d.fail(req.Op, ferr)
			}
			return d.fail(req.Op, &Error{Code: ErrNeedsManualReconcile, Message: d.m.Name + ": create accepted but the resource is not listed under its tag"})
		}
		obj, err := d.extract(ep, body)
		if err != nil {
			return d.fail(req.Op, err)
		}
		return d.ok(req.Op, &Result{Resource: d.resource(obj)})
	}
	if !declMaybeApplied(err) || ctx.Err() != nil {
		return d.fail(req.Op, err)
	}
	if r, ferr := d.findByKey(ctx, req.IdempotencyKey); ferr == nil && r != nil {
		return d.ok(req.Op, &Result{Resource: r})
	}
	return d.fail(req.Op, err)
}

// owned inspects req.ResourceID and refuses unless it carries this
// owner's lease tags. It also returns what the provider reports as
// retained (billed after stop/destroy).
func (d *declarative) owned(ctx context.Context, req Request) (*Resource, []string, Result, error) {
	ep := d.doc.Operations[OpInspect]
	if ep == nil {
		r, e := Failure(req.Op, ErrUnsupportedCapability, "%s: %s needs an inspect endpoint", d.m.Name, req.Op)
		return nil, nil, r, e
	}
	var body any
	if err := d.call(ctx, Request{Op: OpInspect, ResourceID: req.ResourceID}, ep, &body); err != nil {
		r, e := d.fail(req.Op, err)
		return nil, nil, r, e
	}
	obj, err := d.extract(ep, body)
	if err != nil {
		r, e := d.fail(req.Op, err)
		return nil, nil, r, e
	}
	res := d.resource(obj)
	if !Owned(res.Tags, d.cfg.Owner) {
		r, e := Failure(req.Op, ErrNotOwned, "%s: resource %s does not carry the %s and %s tags; refusing to act on it", d.m.Name, res.ID, TagLease, OwnerTag(d.cfg.Owner))
		return nil, nil, r, e
	}
	return res, d.retained(obj), Result{}, nil
}

// action runs start/stop/reboot/resize on an owned resource. When the
// resource is already in want no call is made. Stop reports that a
// stopped resource keeps billing.
func (d *declarative) action(ctx context.Context, req Request, want State) (Result, error) {
	res, retained, r, err := d.owned(ctx, req)
	if err != nil {
		return r, err
	}
	done := want != "" && res.State == want
	if req.Op == OpResize && req.Spec != nil && res.Size == req.Spec.Size {
		done = true
	}
	if !done {
		ep := d.doc.Operations[req.Op]
		var body any
		if err := d.call(ctx, req, ep, &body); err != nil {
			return d.fail(req.Op, err)
		}
		if ep.NoResource {
			if res, retained, r, err = d.owned(ctx, req); err != nil {
				return r, err
			}
		} else {
			obj, err := d.extract(ep, body)
			if err != nil {
				return d.fail(req.Op, err)
			}
			res = d.resource(obj)
			if rt := d.retained(obj); rt != nil {
				retained = rt
			}
		}
	}
	out := &Result{Resource: res}
	if req.Op == OpStop {
		out.Cost = &CostReport{
			Ongoing:  true,
			Retained: append([]string{"resource:" + req.ResourceID}, retained...),
			Note:     "a stopped resource keeps its disk, addresses and charge until destroyed",
		}
	}
	return d.ok(req.Op, out)
}

func (d *declarative) destroy(ctx context.Context, req Request) (Result, error) {
	res, retained, r, err := d.owned(ctx, req)
	if err != nil {
		return r, err
	}
	if e := AuthorizeDestroy(req, *res); e != nil {
		return Result{ProtocolVersion: ProtocolVersion, Op: req.Op, Error: e, Cost: RefusedDestroyCost()}, e
	}
	ep := d.doc.Operations[OpDestroy]
	var body any
	if err := d.call(ctx, req, ep, &body); err != nil {
		return d.fail(req.Op, err)
	}
	if !ep.NoResource && body != nil {
		if obj, err := d.extract(ep, body); err == nil {
			if rt := d.retained(obj); rt != nil {
				retained = rt
			}
		}
	}
	res.State = StateDeleted
	cost := &CostReport{Ongoing: len(retained) > 0, Retained: retained}
	if cost.Ongoing {
		cost.Note = "retained attachments are not deleted with the resource and keep billing"
	}
	return d.ok(req.Op, &Result{Resource: res, Cost: cost})
}
