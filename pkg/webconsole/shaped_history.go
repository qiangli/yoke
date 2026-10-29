// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	shapegate "github.com/qiangli/yoke/pkg/shape/gate"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

// The history view: the shaped app's page gains a version list beside the
// candidate panel. It calls the pkg/src exported API only — History, Diff,
// Name, Restore and the existing preview path — so there is exactly one
// history store. Restores verify the target first and then commit forward
// through the same sealed-evidence gate an approval passes; refusals leave
// the active revision untouched.

// historyVersion is one version as the history view serves it: the src
// record plus the names pinned to it and whether it is the active one.
type historyVersion struct {
	Activation string    `json:"activation"`
	Revision   string    `json:"revision"`
	Kind       string    `json:"kind"`
	Request    string    `json:"request,omitempty"`
	Verdict    string    `json:"verdict,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	Principal  string    `json:"principal,omitempty"`
	Author     string    `json:"author,omitempty"`
	When       time.Time `json:"when,omitempty"`
	Names      []string  `json:"names,omitempty"`
	Active     bool      `json:"active,omitempty"`
}

// historyView is the GET history response: every version newest-first with
// who, when, request text, verifier result, evidence reference, names and
// the active marker, plus the raw named and review labels.
type historyView struct {
	App       string              `json:"app"`
	Principal string              `json:"principal"`
	Line      string              `json:"line"`
	Tip       string              `json:"tip,omitempty"`
	Active    string              `json:"active,omitempty"`
	Versions  []historyVersion    `json:"versions,omitempty"`
	Named     []src.NamedVersion  `json:"named,omitempty"`
	Reviews   []src.ReviewVersion `json:"reviews,omitempty"`
}

func toHistoryView(h src.VersionHistory) historyView {
	names := map[string][]string{}
	for _, n := range h.Named {
		names[n.Revision] = append(names[n.Revision], n.Name)
	}
	v := historyView{
		App: h.App, Principal: h.Principal, Line: h.Line,
		Tip: h.Tip, Active: h.Active, Named: h.Named, Reviews: h.Reviews,
	}
	for _, e := range h.Entries {
		v.Versions = append(v.Versions, historyVersion{
			Activation: e.Activation, Revision: e.Revision, Kind: e.Kind,
			Request: e.Request, Verdict: e.Verdict, Evidence: e.Evidence,
			Principal: e.Principal, Author: e.Author, When: e.When,
			Names: names[e.Revision], Active: e.Revision == h.Active && h.Active != "",
		})
	}
	return v
}

// srcHome is the bashy home the src checkout and the per-principal state
// share. The shaping store (shapedApp.store) and the production activator
// resolve it the same way, so history verbs and approvals meet at one
// checkout; a mismatch fails loud at the route, never silently elsewhere.
func (a *shapedApp) srcHome() (string, error) {
	if pa, ok := a.opts.Activator.(*productionActivator); ok && pa.home != "" {
		return pa.home, nil
	}
	return a.opts.home()
}

// srcRepo refuses a history verb when the panel's source is not the managed
// src checkout for this home: versions would resolve in a different
// repository than the one the app is served from.
func (a *shapedApp) srcRepo() (home string, err error) {
	home, err = a.srcHome()
	if err != nil {
		return "", err
	}
	want := filepath.Join(src.SrcDir(home), a.name)
	got := a.repo
	if resolved, rerr := filepath.EvalSymlinks(want); rerr == nil {
		want = resolved
	}
	if abs, aerr := filepath.Abs(got); aerr == nil {
		got = abs
	}
	if resolved, rerr := filepath.EvalSymlinks(got); rerr == nil {
		got = resolved
	}
	if got != want {
		return "", fmt.Errorf("shaped app %s source is %s, want the managed src checkout %s", a.name, got, want)
	}
	return home, nil
}

// resolveHistoryVersion maps anything the caller may name — a full commit, a
// label they pinned, a review id — to the content revision, but only among
// versions on THEIR OWN line. Anything else, including another principal's
// versions, is simply unknown: 404, never a cross-principal read.
func resolveHistoryVersion(h src.VersionHistory, rev string) (string, bool) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", false
	}
	for _, e := range h.Entries {
		if rev == e.Activation || rev == e.Revision {
			return e.Revision, true
		}
	}
	for _, n := range h.Named {
		if rev == n.Name {
			return n.Revision, true
		}
	}
	for _, r := range h.Reviews {
		if rev == r.ID {
			return r.Revision, true
		}
	}
	return "", false
}

func (s *server) historyFor(w http.ResponseWriter, r *http.Request) (*shapedApp, string, string, src.VersionHistory, bool) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return nil, "", "", src.VersionHistory{}, false
	}
	home, err := a.srcRepo()
	if err != nil {
		http.Error(w, "version history unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return nil, "", "", src.VersionHistory{}, false
	}
	h, err := src.History(home, a.name, principal)
	if err != nil {
		http.Error(w, "version history unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return nil, "", "", src.VersionHistory{}, false
	}
	return a, principal, home, h, true
}

func (s *server) handleShapeHistory(w http.ResponseWriter, r *http.Request) {
	_, _, _, h, ok := s.historyFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toHistoryView(h))
}

func (s *server) handleShapeHistoryDiff(w http.ResponseWriter, r *http.Request) {
	a, _, _, h, ok := s.historyFor(w, r)
	if !ok {
		return
	}
	from, okFrom := resolveHistoryVersion(h, r.URL.Query().Get("from"))
	to, okTo := resolveHistoryVersion(h, r.URL.Query().Get("to"))
	if !okFrom || !okTo {
		http.Error(w, "unknown version: name two versions from your own history", http.StatusNotFound)
		return
	}
	frag, err := renderDiff(a.repo, from, to)
	if err != nil {
		http.Error(w, "diff unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, frag)
}

func decodeHistoryBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "send a JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *server) handleShapeHistoryName(w http.ResponseWriter, r *http.Request) {
	_, principal, home, h, ok := s.historyFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Rev  string `json:"rev"`
		Name string `json:"name"`
	}
	if !decodeHistoryBody(w, r, &in) {
		return
	}
	target, ok := resolveHistoryVersion(h, in.Rev)
	if !ok {
		http.Error(w, "unknown version: name a version from your own history", http.StatusNotFound)
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		http.Error(w, "send {\"rev\": \"<version>\", \"name\": \"<label>\"}", http.StatusBadRequest)
		return
	}
	ref, err := src.Name(home, h.App, principal, target, strings.TrimSpace(in.Name))
	if err != nil {
		if strings.Contains(err.Error(), "already names") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": in.Name, "ref": ref, "revision": target,
	})
}

// restoreGate is the activation gate a history restore passes: the shared
// production gate the production activator runs before switching an
// approval. Evidence sealed by the pre-restore verification below is what
// satisfies it; anything else refuses with the evidence reason.
func restoreGate(home, evidence string) shape.ActivationGate {
	return shapegate.New(home, evidence)
}

// verifyRestoreTarget runs the console verifier over the restore target
// against the current line tip, sealing fresh evidence bound to exactly
// that pair. The workspace is a throwaway clone: the canonical checkout is
// never touched and the active revision does not move.
func (s *server) verifyRestoreTarget(ctx context.Context, a *shapedApp, principal, target, base string) (Verdict, error) {
	if a.opts.Verifier == nil {
		return Verdict{}, errors.New("no verifier is configured on this console; nothing can be restored")
	}
	work, err := os.MkdirTemp("", "shape-restore-*")
	if err != nil {
		return Verdict{}, err
	}
	defer os.RemoveAll(work)
	dir := filepath.Join(work, "work")
	wr, err := cloneAt(ctx, a.repo, dir, target)
	if err != nil {
		return Verdict{}, fmt.Errorf("prepare restore workspace: %w", err)
	}
	// The verifier clones this workspace whole and detaches at the base and
	// then the target, so both must be on branches here: the candidate
	// branch is the verifier's own, the base branch is ours.
	if err := wr.Storer.SetReference(plumbing.NewHashReference("refs/heads/restore-base", plumbing.NewHash(base))); err != nil {
		return Verdict{}, fmt.Errorf("prepare restore workspace: %w", err)
	}
	return a.opts.Verifier.Verify(ctx, VerifyRequest{
		App: a.name, Principal: principal, SourceDir: a.repo, CandidateDir: dir,
		BaseRev: base, CandidateRev: target, Agent: "history-restore",
	})
}

// restoreVersion verifies the target and records the restore as a new
// forward commit through the evidence gate. Every refusal — unknown
// version, failed verification, schema incompatibility, gate refusal —
// returns the reason and leaves the active revision unchanged. When undo is
// true the request line is "undo"; otherwise it is "restore to <name or
// short sha>" derived from the caller's rev label.
func (s *server) restoreVersion(ctx context.Context, a *shapedApp, principal, home string, h src.VersionHistory, rev string, undo bool) (src.ActivateResult, error) {
	var out src.ActivateResult
	target, ok := resolveHistoryVersion(h, rev)
	if !ok {
		return out, errUnknownHistoryVersion
	}
	if h.Tip == "" {
		return out, errors.New("restore refused: no versions are recorded on your line yet")
	}
	v, err := s.verifyRestoreTarget(ctx, a, principal, target, h.Tip)
	if err != nil {
		return out, err
	}
	if !v.Pass {
		reason := strings.TrimSpace(v.Detail)
		if reason == "" {
			reason = "the verifier did not pass this version"
		}
		return out, fmt.Errorf("restore refused: %s", reason)
	}
	request := "restore to " + restoreLabel(h, rev, target)
	if undo {
		request = "undo"
	}
	return src.Restore(home, a.name, target, principal, src.RestoreOptions{Gate: restoreGate(home, v.Evidence), Request: request})
}

// restoreLabel names the restore target the way the request line does: the
// caller's label when they named one, else the short SHA.
func restoreLabel(h src.VersionHistory, rev, target string) string {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return shortRev(target)
	}
	for _, n := range h.Named {
		if rev == n.Name {
			return rev
		}
	}
	for _, e := range h.Entries {
		if rev == e.Activation {
			return shortRev(target)
		}
	}
	if len(rev) >= 7 && isHexRev(rev) {
		return shortRev(target)
	}
	return rev
}

func shortRev(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func isHexRev(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' {
			continue
		}
		return false
	}
	return true
}

var errUnknownHistoryVersion = errors.New("unknown version: restore a version from your own history")

func writeRestore(w http.ResponseWriter, h src.VersionHistory, res src.ActivateResult) {
	active := res.Revision
	if active == "" {
		active = h.Active
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app": h.App, "principal": res.Principal,
		"revision": res.Revision, "activation": res.Activation,
		"previous": res.Previous, "active": active,
	})
}

func (s *server) handleShapeHistoryRestore(w http.ResponseWriter, r *http.Request) {
	a, principal, home, h, ok := s.historyFor(w, r)
	if !ok {
		return
	}
	var in struct {
		Rev string `json:"rev"`
	}
	if !decodeHistoryBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Rev) == "" {
		http.Error(w, "send {\"rev\": \"<version>\"}", http.StatusBadRequest)
		return
	}
	res, err := s.restoreVersion(r.Context(), a, principal, home, h, in.Rev, false)
	if err != nil {
		if errors.Is(err, errUnknownHistoryVersion) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeRestore(w, h, res)
}

// handleShapeHistoryUndo restores the previous active revision — the undo of
// a restore is a restore. With nothing to undo it refuses and the active
// revision stays where it is.
func (s *server) handleShapeHistoryUndo(w http.ResponseWriter, r *http.Request) {
	a, principal, home, h, ok := s.historyFor(w, r)
	if !ok {
		return
	}
	st, err := a.store(principal)
	if err != nil {
		http.Error(w, "version history unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	state, err := st.Read()
	if err != nil {
		http.Error(w, "version history unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(state.LastKnownGood) == "" {
		http.Error(w, "nothing to undo: no previous version is recorded", http.StatusConflict)
		return
	}
	res, err := s.restoreVersion(r.Context(), a, principal, home, h, state.LastKnownGood, true)
	if err != nil {
		if errors.Is(err, errUnknownHistoryVersion) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeRestore(w, h, res)
}

// handleShapeHistoryPreview serves one history version exactly as the app
// would be served, for its owner only — the existing preview path
// (serveTree over a private copy of the owner's data), so a preview can
// never move the active version or touch real data.
func (s *server) handleShapeHistoryPreview(w http.ResponseWriter, r *http.Request) {
	a, principal, _, h, ok := s.historyFor(w, r)
	if !ok {
		return
	}
	rev := r.PathValue("rev")
	target, ok := resolveHistoryVersion(h, rev)
	if !ok {
		http.NotFound(w, r)
		return
	}
	repo, err := a.open()
	if err != nil {
		http.Error(w, "version unavailable", http.StatusInternalServerError)
		return
	}
	commit, err := repo.CommitObject(plumbing.NewHash(target))
	if err != nil {
		http.Error(w, "version unavailable", http.StatusInternalServerError)
		return
	}
	key := "history-" + target
	if len(key) > 20 {
		key = key[:20]
	}
	data, err := a.previewData(principal, key)
	if err != nil {
		http.Error(w, "preview data unavailable", http.StatusServiceUnavailable)
		return
	}
	prefix := "/shape/" + a.name + "/history/" + rev
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		http.Redirect(w, r, prefix+"/", http.StatusFound)
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = rest
	a.serveTree(w, r2, commit, mountRel(r2), principal, data)
}

// historyRoutes registers the version-history view beside the candidate
// routes. Every route starts at shapeCaller — the same signed-in-principal
// gate as the shape routes — and the console gate applies the same
// session/scope rules (a device scoped to the app reaches its own history;
// anything else is refused before a handler runs).
func (s *server) historyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/shape/{app}/history", s.handleShapeHistory)
	mux.HandleFunc("GET /api/shape/{app}/history/diff", s.handleShapeHistoryDiff)
	mux.HandleFunc("POST /api/shape/{app}/history/name", s.handleShapeHistoryName)
	mux.HandleFunc("POST /api/shape/{app}/history/restore", s.handleShapeHistoryRestore)
	mux.HandleFunc("POST /api/shape/{app}/history/undo", s.handleShapeHistoryUndo)
	mux.HandleFunc("/shape/{app}/history/{rev}/", s.handleShapeHistoryPreview)
}
