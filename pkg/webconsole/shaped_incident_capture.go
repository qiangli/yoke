// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Self-repair, slice A: error capture and the fixing page (S549.R1).
//
// When a shaped app's api unit or declared server fails for a signed-in
// principal, the console records an Incident under that principal's own
// shaping state (shaped_incident.go) and answers so the page can follow the
// repair: an api/* fetch gets JSON carrying the incident id with the
// original status, and an HTML navigation gets the fixing page (503 with
// Retry-After) instead of the bare error. 403/404/413 are not incidents —
// there is nothing a repair could fix — and without a principal nothing is
// recorded and every error answers exactly as before.
//
// Slice B owns what happens next: when Observe says a repair should start,
// this slice calls the startRepair hook and stops. The repair pipeline
// itself is not here.

// startRepair (shaped_repair.go) starts the repair; capture never blocks on it.

// recordIncident records one serving error for principal and reports whether
// an incident was recorded. Empty principal (or an unusable store) records
// nothing: the caller then answers the bare error as before.
func (a *shapedApp) recordIncident(principal string, kind IncidentKind, rel string, status int, msg, evidence, baseRev string) (Incident, bool) {
	if principal == "" || a.replay {
		return Incident{}, false
	}
	st, err := a.store(principal)
	if err != nil {
		return Incident{}, false
	}
	s, err := openIncidentsWithLimit(st, a.repairLimitForRev(baseRev))
	if err != nil {
		return Incident{}, false
	}
	in, start, err := s.Observe(Incident{
		App: a.name, Kind: kind, Path: rel, Status: status,
		Message: incidentLine(msg), Evidence: redactEvidence(evidence), BaseRev: baseRev,
	}, time.Now())
	if err != nil {
		return Incident{}, false
	}
	if start && startRepair != nil {
		startRepair(a, principal, in)
	}
	return in, true
}

// incidentLine is the one-line error the user would see.
func incidentLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// secretValue masks a credential-looking assignment: the key stays so the
// line is still diagnosable, the value does not.
var secretValue = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|auth|cookie|session|credential|private[_-]?key)\s*([:=])\s*\S+`)

var secretHeader = regexp.MustCompile(`(?im)^\s*(cookie|authorization|proxy-authorization|x-api-key)\s*:\s*.*$`)

// redactEvidence bounds and scrubs evidence before it is stored. Evidence is
// ever only a unit's stderr tail, a server's stderr tail, or a page's
// message/stack — never request bodies, headers, cookies or env — and the
// masks are the belt to those braces: anything shaped like a credential
// keeps its key and loses its value.
func redactEvidence(s string) string {
	if s == "" {
		return ""
	}
	s = secretHeader.ReplaceAllString(s, "$1: [redacted]")
	s = secretValue.ReplaceAllString(s, "${1}${2}[redacted]")
	if len(s) > incidentEvidenceLimit {
		s = s[len(s)-incidentEvidenceLimit:]
	}
	return s
}

// wantsFixingPage reports whether r is an HTML navigation of the app itself:
// a GET for a non-api/* path that accepts HTML. Only those get the fixing
// page; api/* fetches get JSON, anything else keeps its bare error.
func wantsFixingPage(r *http.Request, rel string) bool {
	if r.Method != http.MethodGet || strings.HasPrefix(rel, "api/") {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// writeIncidentJSON answers an api/* fetch that recorded an incident: the
// original status with the one-line error and the incident to follow.
func writeIncidentJSON(w http.ResponseWriter, status int, msg string, in Incident) {
	writeJSON(w, status, map[string]any{
		"error": incidentLine(msg), "incident": in.ID, "state": string(in.State),
	})
}

// writeAPIError answers a failed api unit run. With a principal the error is
// recorded and the fetch gets JSON carrying the incident; without one the
// bare error answers exactly as before. baseRev is the served revision and
// "" for non-serving callers, which never record.
func (a *shapedApp) writeAPIError(w http.ResponseWriter, name, principal string, status int, msg, evidence, baseRev string) {
	if baseRev == "" {
		http.Error(w, msg, status)
		return
	}
	if in, ok := a.recordIncident(principal, IncidentAPI, "api/"+name, status, msg, evidence, baseRev); ok {
		writeIncidentJSON(w, status, msg, in)
		return
	}
	http.Error(w, msg, status)
}

// writeServerError answers a failed declared server: build/start failures
// and proxy errors funnel here. With a principal the error is recorded and
// the response follows the request kind — JSON for api/* with the original
// status, the fixing page (503, Retry-After) for an HTML navigation, the
// bare error otherwise. Without one the bare error answers as before.
func (a *shapedApp) writeServerError(w http.ResponseWriter, r *http.Request, principal string, status int, msg, evidence, baseRev string) {
	rel := mountRel(r)
	in, ok := a.recordIncident(principal, IncidentServer, rel, status, msg, evidence, baseRev)
	if !ok {
		http.Error(w, msg, status)
		return
	}
	if strings.HasPrefix(rel, "api/") {
		writeIncidentJSON(w, status, msg, in)
		return
	}
	if wantsFixingPage(r, rel) {
		writeRepairPage(w, r, a, in, msg)
		return
	}
	http.Error(w, msg, status)
}

// writeServerUpstream records an upstream 5xx the proxy saw and reshapes the
// response the same way writeServerError does: JSON for api/* with the
// upstream status, the fixing page for an HTML navigation, untouched
// otherwise. Without a principal nothing is recorded and the response passes
// through byte for byte.
func (a *shapedApp) writeServerUpstream(resp *http.Response, principal, baseRev string, stderr string) {
	if resp.Request == nil {
		return
	}
	rel := mountRel(resp.Request)
	msg := "server answered " + displayRel(rel) + " with " + http.StatusText(resp.StatusCode)
	in, ok := a.recordIncident(principal, IncidentServer, rel, resp.StatusCode, msg, stderr, baseRev)
	if !ok {
		return
	}
	var body []byte
	var contentType string
	status := resp.StatusCode
	if strings.HasPrefix(rel, "api/") {
		contentType = "application/json; charset=utf-8"
		body, _ = json.Marshal(map[string]any{
			"error": incidentLine(msg), "incident": in.ID, "state": string(in.State),
		})
	} else if wantsFixingPage(resp.Request, rel) {
		contentType = "text/html; charset=utf-8"
		body = repairPage(resp.Request, a, in, msg)
		status = http.StatusServiceUnavailable
	} else {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Header.Set("Content-Type", contentType)
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding")
	if status == http.StatusServiceUnavailable {
		resp.Header.Set("Retry-After", "10")
	}
}

// displayRel is rel for messages: the app root shows as "/".
func displayRel(rel string) string {
	if rel == "" {
		return "/"
	}
	return rel
}

// repairPage renders the fixing page: small, in the launcher's plain style,
// carrying the incident id, the Genie component (which polls the incident
// and reloads when the repair lands), and a bootstrap naming the incident.
func repairPage(r *http.Request, a *shapedApp, in Incident, msg string) []byte {
	var title, text, extra string
	switch in.State {
	case IncidentFailed:
		title = "Could not fix " + a.name
		text = "Bashy could not fix the error, so the previous version is still active."
		extra = `<p class="inc">Incident ` + html.EscapeString(in.ID) + `</p>`
	case IncidentRefused:
		title = "A fix is waiting for review"
		text = "The fix changes " + a.name + ", so it is waiting for your review instead of going live on its own."
		review := launcherBase(r) + "shape/" + url.PathEscape(a.name) + "/"
		extra = `<p><a href="` + html.EscapeString(review) + `">Review the fix</a></p><p class="inc">Incident ` + html.EscapeString(in.ID) + `</p>`
	default:
		title = "Fixing " + a.name
		text = "Something broke while serving this page. Bashy saw the error and is working on a fix — this page comes back on its own when the fix lands."
		extra = `<p class="inc">Incident ` + html.EscapeString(in.ID) + `</p>`
	}
	tag := genieTagFor(r, a.name, a.mount, nil, geniePage("/"+displayRel(mountRel(r)), r.URL.RawQuery), "")
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>` + html.EscapeString(title) + `</title>` +
		`<style>body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;color:#18181b;background:#fafafa;margin:0;display:flex;align-items:center;justify-content:center;min-height:100vh}main{max-width:32rem;padding:2rem}h1{font-size:1.25rem;margin:0 0 .5rem}p{margin:.5rem 0;color:#52525b}.inc{font-size:.8rem;color:#a1a1aa}</style>` +
		`</head><body><main><h1>` + html.EscapeString(title) + `</h1><p>` + html.EscapeString(text) + `</p>`)
	if msg != "" && in.State != IncidentRepairing {
		b.WriteString(`<p>` + html.EscapeString(incidentLine(msg)) + `</p>`)
	}
	b.WriteString(extra)
	b.WriteString(`<script>window.__bashyIncident="` + html.EscapeString(in.ID) + `";</script>` + "\n")
	b.WriteString(tag.html())
	b.WriteString(`</main></body></html>`)
	return []byte(b.String())
}

// writeRepairPage answers an HTML navigation with the fixing page: 503 so
// the failure stays a failure, Retry-After so a client without the component
// still comes back.
func writeRepairPage(w http.ResponseWriter, r *http.Request, a *shapedApp, in Incident, msg string) {
	body := repairPage(r, a, in, msg)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "10")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}

// serverCapture builds the proxy hooks that record upstream failures for
// principal under baseRev. p supplies the server's stderr tail as evidence.
func (a *shapedApp) serverCapture(principal, baseRev string, p *serverProc) *serverErrorCapture {
	return &serverErrorCapture{
		upstream: func(resp *http.Response) {
			var tail string
			if p != nil && p.stderr != nil {
				tail = p.stderr.String()
			}
			a.writeServerUpstream(resp, principal, baseRev, tail)
		},
		fail: func(w http.ResponseWriter, r *http.Request, err error) {
			var tail string
			if p != nil && p.stderr != nil {
				tail = p.stderr.String()
			}
			a.writeServerError(w, r, principal, http.StatusBadGateway, "shaped app server unreachable: "+err.Error(), tail, baseRev)
		},
	}
}

// ---- page-reported incidents -----------------------------------------------

// incidentReportLimit caps a page's error report: 8 KB, refused past it.
const incidentReportLimit = 8 << 10

// incidentReport is what the page posts: a failed same-origin asset, an
// uncaught script error, or an unhandled rejection. url names the failed
// asset for asset reports; offline says the page had no network at all.
type incidentReport struct {
	Kind    IncidentKind `json:"kind"`
	Path    string       `json:"path"`
	Message string       `json:"message"`
	Stack   string       `json:"stack"`
	URL     string       `json:"url"`
	Offline bool         `json:"offline"`
}

// handleShapeIncidentReport records a page-reported asset/script error for
// the requesting principal. Kind is asset|script only; the body is capped at
// 8 KB and the message/stack truncated. A cross-origin or offline report is
// outside bashy's control: recorded as external, never repaired.
func (s *server) handleShapeIncidentReport(w http.ResponseWriter, r *http.Request) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, incidentReportLimit))
	if err != nil {
		http.Error(w, "incident report too large", http.StatusRequestEntityTooLarge)
		return
	}
	var rep incidentReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		http.Error(w, "incident report is not JSON", http.StatusBadRequest)
		return
	}
	in, start, err := observeIncidentReport(a, principal, rep, rep.Offline || crossOrigin(r, rep.URL))
	if err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, errIncidentKind) {
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}
	if start && startRepair != nil {
		startRepair(a, principal, in)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"incident": in.ID, "state": string(in.State)})
}

// crossOrigin reports whether raw names an asset outside the request's own
// origin: an absolute URL whose host is not this host. Relative URLs are the
// app's own.
func crossOrigin(r *http.Request, raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return false
	}
	host := r.Host
	if h := strings.TrimSpace(u.Hostname()); h != "" {
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		return !strings.EqualFold(h, strings.Trim(host, "[]"))
	}
	return false
}

// handleShapeIncident answers one of the principal's own incidents: its state
// and attempts, for the page polling a repair. Another principal's id is
// simply not in this store: 404.
func (s *server) handleShapeIncident(w http.ResponseWriter, r *http.Request) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return
	}
	st, err := a.store(principal)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	istore, err := openIncidents(st)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	in, err := istore.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func observeIncidentReport(a *shapedApp, principal string, rep incidentReport, external bool) (Incident, bool, error) {
	if rep.Kind != IncidentAsset && rep.Kind != IncidentScript {
		return Incident{}, false, errIncidentKind
	}
	path := rep.Path
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimSpace(strings.TrimPrefix(path, "/"))
	if len(path) > 256 {
		path = path[:256]
	}
	if path == "" {
		path = "/"
	}
	msg := incidentLine(rep.Message)
	if len(msg) > 1024 {
		msg = msg[:1024]
	}
	stack := strings.TrimSpace(rep.Stack)
	if len(stack) > 2048 {
		stack = stack[len(stack)-2048:]
	}
	if msg == "" {
		msg = incidentLine(string(rep.Kind) + " error: " + rep.URL)
		if msg == "" {
			msg = string(rep.Kind) + " error"
		}
	}
	state := IncidentStatus("")
	if external {
		state = IncidentExternal
	}
	baseRev := ""
	if repo, err := a.open(); err == nil {
		if c, err := a.active(repo, principal); err == nil {
			baseRev = c.Hash.String()
		}
	}
	st, err := a.store(principal)
	if err != nil {
		return Incident{}, false, err
	}
	istore, err := openIncidentsWithLimit(st, a.repairLimitForRev(baseRev))
	if err != nil {
		return Incident{}, false, err
	}
	in, start, err := istore.Observe(Incident{
		App: a.name, Kind: rep.Kind, Path: path,
		Message: msg, Evidence: redactEvidence(msg + "\n" + stack),
		BaseRev: baseRev, State: state,
	}, time.Now())
	if err != nil {
		return Incident{}, false, err
	}

	return in, start, nil
}

var errIncidentKind = errors.New("incident kind must be asset or script")
