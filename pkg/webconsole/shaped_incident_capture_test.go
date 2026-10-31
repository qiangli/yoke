// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

// S549.R1a slice A: server-side error capture for self-repair. A broken api
// unit records an incident for the requesting principal only; an HTML
// navigation gets the fixing page while an api/* fetch gets JSON carrying
// the incident id; 403/404/413 are never incidents; evidence never carries
// request bodies or cookies; page-reported asset/script errors arrive at
// POST api/shape/{app}/incidents and are readable at GET .../incidents/{id}.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"

	"github.com/qiangli/yoke/pkg/shape/contain"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/websession"
)

type incidentCaptureEnv struct {
	t      *testing.T
	h      http.Handler
	home   string
	source string
	alice  http.Header
	bob    http.Header
}

func newIncidentCaptureEnv(t *testing.T) *incidentCaptureEnv {
	t.Helper()
	home := t.TempDir()
	work := filepath.Join(t.TempDir(), "shape-work")
	source := newNotesSource(t)
	for _, user := range []string{"alice", "bob"} {
		st, err := shape.OpenAt(home, "notes", "device:"+user)
		if err != nil {
			t.Fatal(err)
		}
		seed, err := os.ReadFile(filepath.Join(notesFixture, "data", user, "notes.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteData("notes.txt", seed); err != nil {
			t.Fatal(err)
		}
	}
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	pairPath := filepath.Join(home, "pairing.json")
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Pairing: true, PairStorePath: pairPath, Panels: []Panel{notes},
		Shape: ShapeOptions{Home: home, WorkRoot: work, Runner: []string{"/bin/sh"}, Level: contain.LevelProcess},
	})
	pairs := newPairStore(pairPath)
	return &incidentCaptureEnv{t: t, h: h, home: home, source: source,
		alice: pairedPrincipal(t, h, pairs, "alice"), bob: pairedPrincipal(t, h, pairs, "bob")}
}

func (e *incidentCaptureEnv) headRev() string {
	e.t.Helper()
	r, err := gogit.PlainOpen(e.source)
	if err != nil {
		e.t.Fatal(err)
	}
	ref, err := r.Head()
	if err != nil {
		e.t.Fatal(err)
	}
	return ref.Hash().String()
}

func (e *incidentCaptureEnv) pinActive(principal, rev string) {
	e.t.Helper()
	st, err := shape.OpenAt(e.home, "notes", principal)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := st.Write(shape.State{ActiveRevision: rev, LastKnownGood: rev}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *incidentCaptureEnv) incidentsOf(principal string) []Incident {
	e.t.Helper()
	st, err := shape.OpenAt(e.home, "notes", principal)
	if err != nil {
		e.t.Fatal(err)
	}
	s, err := openIncidents(st)
	if err != nil {
		e.t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

type apiErrorBody struct {
	Error    string `json:"error"`
	Incident string `json:"incident"`
	State    string `json:"state"`
}

func TestIncidentCaptureBrokenAPI(t *testing.T) {
	e := newIncidentCaptureEnv(t)
	const peer = "192.0.2.20:5555"
	wideDir := filepath.Join(t.TempDir(), "granted")
	if err := os.MkdirAll(wideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A unit the principal may not run: the 403 path, never an incident.
	commitFile(t, e.source, "api/wide.md", "# wide\n\n```bsh\n@writes('"+filepath.ToSlash(wideDir)+"')\nsave() { echo saved; }\nsave\n```\n")
	e.pinActive("device:bob", e.headRev())
	broken := commitFile(t, e.source, "api/list.md", "# list\n\n```bsh\necho unit-boom-marker >&2\nexit 3\n```\n")
	e.pinActive("device:alice", broken)

	var repaired []Incident
	var repairedBy []string
	oldHook := startRepair
	startRepair = func(_ *shapedApp, principal string, in Incident) {
		repaired = append(repaired, in)
		repairedBy = append(repairedBy, principal)
	}
	t.Cleanup(func() { startRepair = oldHook })

	// Alice's broken fetch, carrying secrets in the body and a cookie: the
	// answer is JSON with the original 500 and the incident id.
	hdr := e.alice.Clone()
	hdr.Set("Content-Type", "text/plain")
	hdr.Add("Cookie", "evil=s3cr3t-cookie-marker")
	w := send(e.h, http.MethodPost, "/notes/api/list", peer, hdr, "secret-body-marker-XYZ")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("broken api = %d, want 500: %s", w.Code, w.Body.String())
	}
	var got apiErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("broken api body is not JSON: %v: %s", err, w.Body.String())
	}
	if !strings.Contains(got.Error, "(exit 3)") || got.Incident == "" || got.State != string(IncidentRepairing) {
		t.Fatalf("broken api body = %+v, want the exit, an incident id and repairing", got)
	}
	if len(repaired) != 1 || repairedBy[0] != "device:alice" || repaired[0].ID != got.Incident {
		t.Fatalf("startRepair calls = %v for %v, want one for device:alice", repaired, repairedBy)
	}

	// The incident is alice's: kind, path, status, base revision, and its
	// evidence is the unit's stderr — never the request body or cookies.
	st, err := shape.OpenAt(e.home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	s, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.Get(got.Incident)
	if err != nil {
		t.Fatalf("Get(%s): %v", got.Incident, err)
	}
	if in.Kind != IncidentAPI || in.Path != "api/list" || in.Status != http.StatusInternalServerError || in.BaseRev != broken {
		t.Fatalf("incident = %+v, want api/api/list/500/%s", in, broken)
	}
	if !strings.Contains(in.Evidence, "unit-boom-marker") {
		t.Fatalf("evidence lacks the unit stderr: %q", in.Evidence)
	}
	for _, secret := range []string{"secret-body-marker-XYZ", "s3cr3t-cookie-marker"} {
		if strings.Contains(in.Evidence, secret) || strings.Contains(in.Message, secret) {
			t.Fatalf("incident carries request data %q: %+v", secret, in)
		}
	}

	// Bob is on the working revision: base behavior, and alice's incident
	// is not his.
	if w := do(e.h, http.MethodGet, "/notes/api/list", peer, e.bob); w.Code != http.StatusOK {
		t.Fatalf("bob list = %d, want base behavior 200: %s", w.Code, w.Body.String())
	}
	if w := do(e.h, http.MethodGet, "/api/shape/notes/incidents/"+got.Incident, peer, e.bob); w.Code != http.StatusNotFound {
		t.Fatalf("bob reads alice incident = %d, want 404", w.Code)
	}
	if list := e.incidentsOf("device:bob"); len(list) != 0 {
		t.Fatalf("bob incidents = %+v, want none", list)
	}

	// 404 and 403 are not incidents: refusal bodies stay bare, and alice
	// still has exactly her one incident.
	if w := do(e.h, http.MethodGet, "/notes/api/nosuch", peer, e.alice); w.Code != http.StatusNotFound {
		t.Fatalf("missing unit = %d, want 404", w.Code)
	}
	if w := do(e.h, http.MethodGet, "/notes/api/wide", peer, e.alice); w.Code != http.StatusForbidden {
		t.Fatalf("wide unit = %d, want 403", w.Code)
	}
	if list := e.incidentsOf("device:alice"); len(list) != 1 || list[0].ID != got.Incident {
		t.Fatalf("alice incidents after 404/403 = %+v, want only %s", list, got.Incident)
	}

	// Alice reads her own incident back: state and attempts.
	w = do(e.h, http.MethodGet, "/api/shape/notes/incidents/"+got.Incident, peer, e.alice)
	if w.Code != http.StatusOK {
		t.Fatalf("GET incident = %d: %s", w.Code, w.Body.String())
	}
	var back Incident
	if err := json.Unmarshal(w.Body.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.State != IncidentRepairing || len(back.Attempts) != 0 {
		t.Fatalf("incident back = %+v, want repairing with no attempts", back)
	}
}

func TestIncidentReportEndpoint(t *testing.T) {
	e := newIncidentCaptureEnv(t)
	const peer = "192.0.2.20:5555"
	post := func(who http.Header, body string, want int) string {
		t.Helper()
		hdr := who.Clone()
		hdr.Set("Content-Type", "application/json")
		w := send(e.h, http.MethodPost, "/api/shape/notes/incidents", peer, hdr, body)
		if w.Code != want {
			t.Fatalf("POST incidents = %d, want %d: %s", w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}

	// A page script error is recorded and readable by its own principal.
	out := post(e.alice, `{"kind":"script","path":"/index.html","message":"TypeError: x is undefined","stack":"at load (notes.js:3)"}`, http.StatusCreated)
	var created struct {
		Incident string `json:"incident"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Incident == "" || created.State != string(IncidentRepairing) {
		t.Fatalf("report = %s, want an incident id in repairing", out)
	}
	w := do(e.h, http.MethodGet, "/api/shape/notes/incidents/"+created.Incident, peer, e.alice)
	if w.Code != http.StatusOK {
		t.Fatalf("GET incident = %d: %s", w.Code, w.Body.String())
	}
	var in Incident
	if err := json.Unmarshal(w.Body.Bytes(), &in); err != nil {
		t.Fatal(err)
	}
	if in.Kind != IncidentScript || in.State != IncidentRepairing || !strings.Contains(in.Evidence, "notes.js:3") {
		t.Fatalf("reported incident = %+v", in)
	}
	// Another principal's eyes only: bob gets 404.
	if w := do(e.h, http.MethodGet, "/api/shape/notes/incidents/"+created.Incident, peer, e.bob); w.Code != http.StatusNotFound {
		t.Fatalf("bob reads alice incident = %d, want 404", w.Code)
	}
	// Anonymous callers get no shaping verb.
	hdr := http.Header{"Content-Type": {"application/json"}}
	if w := send(e.h, http.MethodPost, "/api/shape/notes/incidents", peer, hdr, `{"kind":"script","message":"x"}`); w.Code != http.StatusForbidden {
		t.Fatalf("anonymous report = %d, want 403", w.Code)
	}

	// Only asset|script are reported by the page.
	post(e.alice, `{"kind":"api","message":"x"}`, http.StatusBadRequest)
	post(e.alice, `{"kind":"","message":"x"}`, http.StatusBadRequest)
	post(e.alice, `{"kind":"script"`, http.StatusBadRequest)
	// Bodies past 8 KB are refused before they are read.
	post(e.alice, `{"kind":"script","message":"`+strings.Repeat("y", 9<<10)+`"}`, http.StatusRequestEntityTooLarge)

	// A cross-origin asset failure is outside bashy's control: reported,
	// never repaired.
	out = post(e.alice, `{"kind":"asset","path":"/index.html","message":"failed to load","url":"https://cdn.example.test/lib.js"}`, http.StatusCreated)
	var ext struct {
		Incident string `json:"incident"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &ext); err != nil || ext.State != string(IncidentExternal) {
		t.Fatalf("cross-origin report = %s, want state external", out)
	}
	w = do(e.h, http.MethodGet, "/api/shape/notes/incidents/"+ext.Incident, peer, e.alice)
	var ein Incident
	_ = json.Unmarshal(w.Body.Bytes(), &ein)
	if ein.State != IncidentExternal {
		t.Fatalf("cross-origin incident = %+v, want external", ein)
	}
	// An offline page says so and is external too.
	out = post(e.alice, `{"kind":"script","path":"/","message":"fetch failed","offline":true}`, http.StatusCreated)
	var off struct {
		Incident string `json:"incident"`
		State    string `json:"state"`
	}
	_ = json.Unmarshal([]byte(out), &off)
	if off.State != string(IncidentExternal) {
		t.Fatalf("offline report = %s, want state external", out)
	}
	// Unknown incidents are 404, whatever the principal.
	if w := do(e.h, http.MethodGet, "/api/shape/notes/incidents/inc-no-such", peer, e.alice); w.Code != http.StatusNotFound {
		t.Fatalf("missing incident = %d, want 404", w.Code)
	}
}

func TestIncidentServerProxyResponses(t *testing.T) {
	home := t.TempDir()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	proc := &serverProc{
		addr:  strings.TrimPrefix(upstream.URL, "http://"),
		ready: make(chan struct{}), exited: make(chan struct{}),
		stderr: &tailBuffer{max: serverStderrTailLen},
	}
	close(proc.ready)
	const principal = "device:alice"
	const baseRev = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	app := &shapedApp{name: "notes", mount: "notes/", opts: ShapeOptions{Home: home}}
	cap := app.serverCapture(principal, baseRev, proc)
	h := shapedServerProxy(proc, nil, cap)

	// An HTML navigation gets the fixing page: 503 with Retry-After, and
	// the incident is recorded for the principal.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	r = withPrincipal(r, principal)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	res := w.Result()
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Fatalf("navigation = %d retry %q, want 503 with Retry-After", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("navigation content type = %q, want html", ct)
	}
	st, err := shape.OpenAt(home, "notes", principal)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Kind != IncidentServer || list[0].Status != http.StatusInternalServerError || list[0].BaseRev != baseRev {
		t.Fatalf("server incidents = %+v, want one server 500", list)
	}

	// An api/* fetch keeps the upstream status and carries JSON with the
	// incident id.
	r = httptest.NewRequest(http.MethodGet, "/api/list", nil)
	r.Header.Set("Accept", "application/json")
	r = withPrincipal(r, principal)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	res = w.Result()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("api fetch = %d, want the upstream 500", res.StatusCode)
	}
	var got apiErrorBody
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil || got.Incident == "" {
		t.Fatalf("api fetch body = %+v, %v: want JSON with an incident", got, err)
	}

	// A dead server answers 502 through the same capture.
	dead := &serverProc{
		addr:  "127.0.0.1:1",
		ready: make(chan struct{}), exited: make(chan struct{}),
		stderr: &tailBuffer{max: serverStderrTailLen},
	}
	close(dead.ready)
	h2 := shapedServerProxy(dead, nil, app.serverCapture(principal, baseRev, dead))
	r = httptest.NewRequest(http.MethodGet, "/api/list", nil)
	r = withPrincipal(r, principal)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, r)
	res = w.Result()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("dead server api = %d, want 502", res.StatusCode)
	}
	var got2 apiErrorBody
	if err := json.NewDecoder(res.Body).Decode(&got2); err != nil || got2.Incident == "" {
		t.Fatalf("dead server body = %+v, %v: want JSON with an incident", got2, err)
	}
}

func TestIncidentWriteServerErrorPages(t *testing.T) {
	home := t.TempDir()
	const principal = "device:alice"
	app := &shapedApp{name: "notes", mount: "notes/", opts: ShapeOptions{Home: home}}
	serve := func(method, target, accept string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, target, nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		r = withPrincipal(r, principal)
		w := httptest.NewRecorder()
		app.writeServerError(w, r, principal, http.StatusBadGateway, "server failed to start:\nboom", "boom", "rev0")
		return w
	}
	// HTML navigation: the fixing page.
	w := serve(http.MethodGet, "/", "text/html")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("navigation = %d, want 503 with Retry-After: %s", w.Code, w.Body.String())
	}
	// api/*: JSON with the original status.
	w = serve(http.MethodGet, "/api/list", "application/json")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("api = %d, want 502", w.Code)
	}
	var got apiErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Incident == "" {
		t.Fatalf("api body = %q: want JSON with an incident", w.Body.String())
	}
	// Anything else keeps the bare error and its status.
	w = serve(http.MethodGet, "/app.js", "text/css")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "server failed to start") {
		t.Fatalf("asset = %d %q, want the bare 502", w.Code, w.Body.String())
	}
	// No principal: behavior unchanged, nothing recorded.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept", "text/html")
	w = httptest.NewRecorder()
	app.writeServerError(w, r, "", http.StatusBadGateway, "server failed to start", "", "rev0")
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "Incident") {
		t.Fatalf("anonymous = %d %q, want the bare error", w.Code, w.Body.String())
	}
	st, err := shape.OpenAt(home, "notes", principal)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("incidents = %d, want 3 (navigation, api, asset)", len(list))
	}
}

func TestIncidentRedactEvidence(t *testing.T) {
	in := "token=hunter2\npassword: s3cret\nnormal line\nCookie: abc\nAuthorization: Bearer xyz\n"
	out := redactEvidence(in)
	for _, secret := range []string{"hunter2", "s3cret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("redacted evidence still carries %q: %q", secret, out)
		}
	}
	if !strings.Contains(out, "normal line") {
		t.Fatalf("redaction dropped a harmless line: %q", out)
	}
	big := strings.Repeat("z", incidentEvidenceLimit+100)
	if len(redactEvidence(big)) != incidentEvidenceLimit {
		t.Fatalf("redacted evidence is not tail-capped")
	}
}
