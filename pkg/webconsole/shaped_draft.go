// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// The edit session (Sprint 526.B1h, docs/sprint-526/document-model-shaping-
// 2026-10-31.md): shaping an app like editing a document. From the common
// Genie button a person starts editing; the app switches to a PRIVATE DRAFT
// of their active version, and every wish from then on is one STEP applied
// on the draft so far — the shaping agent works on the draft's tip and its
// result is one commit on top. The steps form a stack with a cursor: undo
// moves the cursor back one step, redo moves it forward, and a new wish
// clears everything past the cursor (recorded as dropped, so Save can still
// list it as undone). Steps are stored commits, so undo and redo never
// re-run the agent. Save verifies the WHOLE draft once — base..tip, through
// the trusted verifier — and on pass activates the tip as ONE new version
// on the person's line whose request text lists every wish (undone ones
// marked); on fail the bar names the failing part and the draft stays
// open. Cancel discards the draft; the active version is untouched either
// way until Save. The draft lives on disk under the principal's candidate
// directory (draft.json + a work clone), so it survives a reload, a closed
// tab and a console restart. A wish from a protected page is filed as a
// request, never a step (the Genie component sends it that way, and the
// draft route refuses it as a step too).

const (
	// draftDirName is the draft's directory under the principal's
	// candidate directory; never a valid candidate id, so candidate
	// listings skip it.
	draftDirName = "draft"
	draftFile    = "draft.json"

	stepRunning = "running"
	stepReady   = "ready"
)

// draftStep is one wish applied on the draft: a commit on top of ParentRev.
type draftStep struct {
	ID       string           `json:"id"`
	Request  string           `json:"request"`
	Page     string           `json:"page,omitempty"`
	Element  string           `json:"element,omitempty"`
	Status   string           `json:"status"`
	Parent   string           `json:"parent_rev"`
	Rev      string           `json:"rev,omitempty"`
	Agent    string           `json:"agent,omitempty"`
	Identity string           `json:"agent_identity,omitempty"`
	Receipt  string           `json:"agent_receipt,omitempty"`
	Coverage *CoverageSummary `json:"coverage,omitempty"`
	Created  time.Time        `json:"created"`
}

// Draft is the stored edit session of one principal on one app.
type Draft struct {
	App       string `json:"app"`
	Principal string `json:"principal"`
	// BaseRev is the active version the draft started from: what Save
	// verifies against and what Cancel leaves untouched.
	BaseRev string `json:"base_rev"`
	// Steps is the stack; Steps[:Cursor] are applied, Steps[Cursor:] are
	// undone and redoable. A running step is always the last one.
	Steps  []draftStep `json:"steps"`
	Cursor int         `json:"cursor"`
	// Dropped are undone steps a later wish cleared from the redo list;
	// Save records them as undone.
	Dropped []draftStep `json:"dropped,omitempty"`
	// LastError is the last wish that produced no step, with why.
	LastError string `json:"last_error,omitempty"`
	// Verdict is the last Save's verifier answer (a refusal keeps the draft).
	Verdict *Verdict  `json:"verdict,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// tip is the revision the draft currently shows.
func (d *Draft) tip() string {
	if d.Cursor <= 0 || d.Cursor > len(d.Steps) {
		return d.BaseRev
	}
	return d.Steps[d.Cursor-1].Rev
}

// busy reports whether the agent is working on a wish.
func (d *Draft) busy() bool {
	return len(d.Steps) > 0 && d.Steps[len(d.Steps)-1].Status == stepRunning
}

func (d *Draft) find(id string) int {
	for i, s := range d.Steps {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// draftView is the draft as the API serves it: the record plus what the
// bar shows (the tip, counts, what is possible right now).
type draftView struct {
	Draft
	Rev     string     `json:"rev"`
	Wishes  int        `json:"wishes"`
	Total   int        `json:"total"`
	Busy    bool       `json:"busy"`
	CanUndo bool       `json:"can_undo"`
	CanRedo bool       `json:"can_redo"`
	Running *draftStep `json:"running,omitempty"`
}

func viewDraft(d Draft) draftView {
	v := draftView{Draft: d, Rev: d.tip(), Wishes: d.Cursor, Total: len(d.Steps), Busy: d.busy()}
	if v.Busy {
		s := d.Steps[len(d.Steps)-1]
		v.Running = &s
		v.Wishes = len(d.Steps) - 1
		v.Total = len(d.Steps) - 1
	}
	v.CanUndo = !v.Busy && d.Cursor > 0
	v.CanRedo = !v.Busy && d.Cursor < len(d.Steps)
	return v
}

// draftRequestText is the one-line request the saved version records:
// every wish in order, undone ones (still on the stack or dropped by a
// later wish) marked as undone.
func draftRequestText(d Draft) string {
	applied := d.Steps[:d.Cursor]
	undone := append(append([]draftStep{}, d.Steps[d.Cursor:]...), d.Dropped...)
	parts := make([]string, 0, len(applied)+len(undone))
	for i, s := range applied {
		parts = append(parts, fmt.Sprintf("%d. %s", i+1, oneLine(s.Request)))
	}
	for _, s := range undone {
		parts = append(parts, "(undone) "+oneLine(s.Request))
	}
	noun := "wishes"
	if len(applied) == 1 {
		noun = "wish"
	}
	return fmt.Sprintf("Edit session, %d %s: %s", len(applied), noun, strings.Join(parts, "; "))
}

var (
	errDraftBusy   = errors.New("Genie is still working on the last wish; wait for it")
	errDraftSaving = errors.New("Save is verifying the draft; wait for it")
	errNoUndo      = errors.New("nothing to undo")
	errNoRedo      = errors.New("nothing to redo")
)

// ---- storage ---------------------------------------------------------------

func (a *shapedApp) draftDir(principal string) (string, error) {
	d, err := a.candidatesDir(principal)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, draftDirName), nil
}

// loadDraft reads the principal's draft; fs.ErrNotExist when there is none.
func (a *shapedApp) loadDraft(principal string) (Draft, string, error) {
	dir, err := a.draftDir(principal)
	if err != nil {
		return Draft{}, "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, draftFile))
	if err != nil {
		return Draft{}, "", err
	}
	var d Draft
	if err := json.Unmarshal(b, &d); err != nil {
		return Draft{}, "", err
	}
	if d.Principal != principal || d.App != a.name {
		return Draft{}, "", fs.ErrNotExist
	}
	if d.Steps == nil {
		d.Steps = []draftStep{}
	}
	return d, dir, nil
}

func saveDraft(dir string, d Draft) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".draft.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, draftFile))
}

// updateDraft applies fn to the stored draft under the hub lock and writes
// it back. fn's error is returned as is and nothing is written.
func (s *server) updateDraft(a *shapedApp, principal string, fn func(d *Draft) error) (Draft, error) {
	s.shapes.mu.Lock()
	defer s.shapes.mu.Unlock()
	d, dir, err := a.loadDraft(principal)
	if err != nil {
		return Draft{}, err
	}
	if err := fn(&d); err != nil {
		return d, err
	}
	d.Updated = time.Now().UTC()
	return d, saveDraft(dir, d)
}

// editDraft is updateDraft for the stack mutations a person makes: refused
// while the agent is working on a wish or Save is verifying the draft.
func (s *server) editDraft(a *shapedApp, principal string, fn func(d *Draft) error) (Draft, error) {
	dir, err := a.draftDir(principal)
	if err != nil {
		return Draft{}, err
	}
	return s.updateDraft(a, principal, func(d *Draft) error {
		if s.shapes.saving[dir] {
			return errDraftSaving
		}
		if d.busy() {
			return errDraftBusy
		}
		return fn(d)
	})
}

// removeDraft deletes the draft and the principal's draft data copy.
func (a *shapedApp) removeDraft(principal, dir string) error {
	if st, err := a.store(principal); err == nil {
		_ = os.RemoveAll(filepath.Join(st.Dir(), "preview", draftDirName))
	}
	return os.RemoveAll(dir)
}

// ---- the session -------------------------------------------------------------

// startDraft opens the principal's edit session from their active version,
// or returns the one already open. created reports which.
func (s *server) startDraft(ctx context.Context, a *shapedApp, principal string) (Draft, bool, error) {
	s.shapes.mu.Lock()
	defer s.shapes.mu.Unlock()
	if d, _, err := a.loadDraft(principal); err == nil {
		return d, false, nil
	}
	repo, err := a.open()
	if err != nil {
		return Draft{}, false, err
	}
	from, err := a.active(repo, principal)
	if err != nil {
		return Draft{}, false, err
	}
	dir, err := a.draftDir(principal)
	if err != nil {
		return Draft{}, false, err
	}
	// A directory without a record is the remains of an interrupted start.
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Draft{}, false, err
	}
	if _, err := cloneAt(ctx, a.repo, filepath.Join(dir, "work"), from.Hash.String()); err != nil {
		_ = os.RemoveAll(dir)
		return Draft{}, false, fmt.Errorf("prepare draft: %w", err)
	}
	now := time.Now().UTC()
	d := Draft{App: a.name, Principal: principal, BaseRev: from.Hash.String(), Steps: []draftStep{}, Created: now, Updated: now}
	if err := saveDraft(dir, d); err != nil {
		_ = os.RemoveAll(dir)
		return Draft{}, false, err
	}
	return d, true, nil
}

// addDraftWish pushes a running step on the stack — clearing the redo
// list first, as in any editor — and runs the agent on the draft's tip in
// the background.
func (s *server) addDraftWish(a *shapedApp, principal string, w wish) (Draft, error) {
	if a.opts.Delegate == nil {
		return Draft{}, errors.New("no shaping agent is configured on this console")
	}
	var idb [8]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return Draft{}, err
	}
	var step draftStep
	d, err := s.editDraft(a, principal, func(d *Draft) error {
		d.Dropped = append(d.Dropped, d.Steps[d.Cursor:]...)
		d.Steps = append([]draftStep{}, d.Steps[:d.Cursor]...)
		step = draftStep{ID: hex.EncodeToString(idb[:]), Request: w.Request, Page: w.Page, Element: w.Element,
			Status: stepRunning, Parent: d.tip(), Created: time.Now().UTC()}
		d.Steps = append(d.Steps, step)
		d.Cursor = len(d.Steps) - 1
		d.LastError = ""
		return nil
	})
	if err != nil {
		return d, err
	}
	s.shapes.wg.Add(1)
	go func() {
		defer s.shapes.wg.Done()
		s.runDraftStep(a, principal, step)
	}()
	return d, nil
}

// runDraftStep is the background half of a wish: the work clone is put at
// the step's parent, the agent runs there, and the result becomes the
// step's commit. A failed wish leaves no step, only the reason.
func (s *server) runDraftStep(a *shapedApp, principal string, step draftStep) {
	ctx := s.opts.Ctx
	dir, err := a.draftDir(principal)
	if err != nil {
		return
	}
	work := filepath.Join(dir, "work")
	fail := func(err error) {
		_, _ = s.updateDraft(a, principal, func(d *Draft) error {
			if i := d.find(step.ID); i >= 0 {
				d.Steps = append(d.Steps[:i:i], d.Steps[i+1:]...)
				if d.Cursor > len(d.Steps) {
					d.Cursor = len(d.Steps)
				}
			}
			d.LastError = oneLine(step.Request) + ": " + err.Error()
			return nil
		})
	}
	repo, err := gogit.PlainOpen(work)
	if err != nil {
		fail(fmt.Errorf("draft workspace: %w", err))
		return
	}
	wt, err := repo.Worktree()
	if err != nil {
		fail(err)
		return
	}
	// The tip of the draft as the stack has it now — after undo, that is
	// an earlier step, and what a previous wish left behind is discarded.
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: plumbing.NewHash(step.Parent), Force: true}); err != nil {
		fail(fmt.Errorf("checkout %s: %w", shortRev(step.Parent), err))
		return
	}
	if err := wt.Clean(&gogit.CleanOptions{Dir: true}); err != nil {
		fail(fmt.Errorf("clean workspace: %w", err))
		return
	}
	res, err := a.opts.Delegate.Delegate(ctx, DelegateRequest{Dir: work,
		Task: shapingTaskFor(a.name, wish{Request: step.Request, Page: step.Page, Element: step.Element})})
	if err != nil {
		fail(fmt.Errorf("shaping agent: %w", err))
		return
	}
	rev, err := commitCandidate(repo, Candidate{Request: step.Request, Principal: principal, Agent: res.Agent, BaseRev: step.Parent})
	if err != nil {
		fail(err)
		return
	}
	_, _ = s.updateDraft(a, principal, func(d *Draft) error {
		i := d.find(step.ID)
		if i < 0 {
			return nil // the draft moved on without this step
		}
		st := &d.Steps[i]
		st.Rev, st.Status = rev, stepReady
		st.Agent, st.Identity, st.Receipt = res.Agent, res.Identity, res.Receipt
		st.Coverage = parseCoverageSummary(res.Output)
		d.Cursor = i + 1
		return nil
	})
}

// moveDraft is undo (delta -1) and redo (+1): the cursor moves, nothing runs.
func (s *server) moveDraft(a *shapedApp, principal string, delta int) (Draft, error) {
	return s.editDraft(a, principal, func(d *Draft) error {
		n := d.Cursor + delta
		if n < 0 {
			return errNoUndo
		}
		if n > len(d.Steps) {
			return errNoRedo
		}
		d.Cursor = n
		return nil
	})
}

// draftSaved is the Save response.
type draftSaved struct {
	App      string `json:"app"`
	Revision string `json:"rev"`
	Request  string `json:"request"`
	Wishes   int    `json:"wishes"`
	Undone   int    `json:"undone"`
}

// saveDraftSession verifies the whole draft once and, on pass, activates
// it as one version listing every wish. A refusal — protected paths, the
// verifier, activation — is returned with the draft still open and the
// verdict recorded on it; a nil error with saved==nil means the draft was
// refused (the view carries why) rather than failed.
func (s *server) saveDraftSession(ctx context.Context, a *shapedApp, principal string) (saved *draftSaved, view draftView, err error) {
	dir, err := a.draftDir(principal)
	if err != nil {
		return nil, view, err
	}
	s.shapes.mu.Lock()
	d, _, err := a.loadDraft(principal)
	if err == nil {
		switch {
		case s.shapes.saving[dir]:
			err = errDraftSaving
		case d.busy():
			err = errDraftBusy
		case d.Cursor == 0:
			err = errors.New("nothing to save: no wish is applied (Cancel discards the draft)")
		default:
			if s.shapes.saving == nil {
				s.shapes.saving = map[string]bool{}
			}
			s.shapes.saving[dir] = true
		}
	}
	s.shapes.mu.Unlock()
	if err != nil {
		return nil, viewDraft(d), err
	}
	defer func() {
		s.shapes.mu.Lock()
		delete(s.shapes.saving, dir)
		s.shapes.mu.Unlock()
	}()

	work := filepath.Join(dir, "work")
	tip := d.tip()
	refuse := func(v Verdict) (*draftSaved, draftView, error) {
		d2, uerr := s.updateDraft(a, principal, func(d *Draft) error { d.Verdict = &v; return nil })
		if uerr != nil {
			return nil, viewDraft(d), uerr
		}
		return nil, viewDraft(d2), nil
	}
	if len(a.protected) > 0 {
		if perr := candidateTouchesProtected(work, d.BaseRev, tip, a.protected); perr != nil {
			return refuse(Verdict{Pass: false, Detail: perr.Error()})
		}
	}
	if a.opts.Verifier == nil {
		return nil, viewDraft(d), errors.New("no verifier is configured on this console; nothing can be saved")
	}
	agent := ""
	if d.Cursor > 0 {
		agent = d.Steps[d.Cursor-1].Agent
	}
	v, err := a.opts.Verifier.Verify(ctx, VerifyRequest{
		App: a.name, Principal: principal, SourceDir: a.repo, CandidateDir: work,
		BaseRev: d.BaseRev, CandidateRev: tip, Agent: agent,
	})
	if err != nil {
		return nil, viewDraft(d), fmt.Errorf("verify: %w", err)
	}
	if !v.Pass {
		return refuse(v)
	}
	if a.opts.Activator == nil {
		return nil, viewDraft(d), errors.New("no activator is configured on this console")
	}
	request := draftRequestText(d)
	err = a.opts.Activator.Activate(ctx, ActivateRequest{
		App: a.name, Principal: principal, SourceDir: a.repo, CandidateDir: work,
		BaseRev: d.BaseRev, CandidateRev: tip, Request: request, Agent: agent, Evidence: v.Evidence,
	})
	if err != nil {
		return refuse(Verdict{Pass: false, Evidence: v.Evidence, Detail: "activation refused: " + err.Error()})
	}
	s.shapes.mu.Lock()
	rerr := a.removeDraft(principal, dir)
	s.shapes.mu.Unlock()
	if rerr != nil {
		slog.Warn("shape: saved draft workspace not removed", "err", rerr)
	}
	return &draftSaved{App: a.name, Revision: tip, Request: request, Wishes: d.Cursor,
		Undone: len(d.Steps) - d.Cursor + len(d.Dropped)}, viewDraft(d), nil
}

// ---- HTTP --------------------------------------------------------------------

// draftFor loads the caller's draft or answers 404.
func (s *server) draftFor(w http.ResponseWriter, r *http.Request) (*shapedApp, string, Draft, string, bool) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return nil, "", Draft{}, "", false
	}
	d, dir, err := a.loadDraft(principal)
	if err != nil {
		http.Error(w, "no draft is open: start editing from the Genie button", http.StatusNotFound)
		return nil, "", Draft{}, "", false
	}
	return a, principal, d, dir, true
}

func (s *server) handleShapeDraftState(w http.ResponseWriter, r *http.Request) {
	if _, _, d, _, ok := s.draftFor(w, r); ok {
		writeJSON(w, http.StatusOK, viewDraft(d))
	}
}

func (s *server) handleShapeDraftStart(w http.ResponseWriter, r *http.Request) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return
	}
	d, created, err := s.startDraft(r.Context(), a, principal)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, viewDraft(d))
}

// draftEditError maps a refused stack mutation to its status.
func draftEditError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "no draft is open: start editing from the Genie button", http.StatusNotFound)
	case errors.Is(err, errDraftBusy), errors.Is(err, errDraftSaving), errors.Is(err, errNoUndo), errors.Is(err, errNoRedo):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
}

func (s *server) handleShapeDraftWish(w http.ResponseWriter, r *http.Request) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return
	}
	in, ok := readWish(w, r)
	if !ok {
		return
	}
	if in.Protected {
		// A protected page is never part of the draft: the wish is filed as
		// a request for the maintainers, exactly as outside a session.
		c, err := s.fileRequest(a, principal, in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusAccepted, c)
		return
	}
	d, err := s.addDraftWish(a, principal, in)
	if err != nil {
		draftEditError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, viewDraft(d))
}

func (s *server) handleShapeDraftUndo(w http.ResponseWriter, r *http.Request) {
	s.handleShapeDraftMove(w, r, -1)
}
func (s *server) handleShapeDraftRedo(w http.ResponseWriter, r *http.Request) {
	s.handleShapeDraftMove(w, r, +1)
}

func (s *server) handleShapeDraftMove(w http.ResponseWriter, r *http.Request, delta int) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return
	}
	d, err := s.moveDraft(a, principal, delta)
	if err != nil {
		draftEditError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewDraft(d))
}

func (s *server) handleShapeDraftSave(w http.ResponseWriter, r *http.Request) {
	a, principal, _, _, ok := s.draftFor(w, r)
	if !ok {
		return
	}
	saved, view, err := s.saveDraftSession(r.Context(), a, principal)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "no draft is open", http.StatusNotFound)
	case errors.Is(err, errDraftBusy), errors.Is(err, errDraftSaving):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil && strings.HasPrefix(err.Error(), "nothing to save"):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case saved == nil:
		// Refused: the draft stays open and the view says why (verdict).
		writeJSON(w, http.StatusConflict, view)
	default:
		writeJSON(w, http.StatusOK, saved)
	}
}

func (s *server) handleShapeDraftCancel(w http.ResponseWriter, r *http.Request) {
	a, principal, d, dir, ok := s.draftFor(w, r)
	if !ok {
		return
	}
	s.shapes.mu.Lock()
	if s.shapes.saving[dir] {
		s.shapes.mu.Unlock()
		http.Error(w, errDraftSaving.Error(), http.StatusConflict)
		return
	}
	err := a.removeDraft(principal, dir)
	s.shapes.mu.Unlock()
	if err != nil {
		http.Error(w, "cancel: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": a.name, "cancelled": true, "wishes": d.Cursor, "base_rev": d.BaseRev})
}

func (s *server) handleShapeDraftDiff(w http.ResponseWriter, r *http.Request) {
	_, _, d, dir, ok := s.draftFor(w, r)
	if !ok {
		return
	}
	frag, err := renderDiff(filepath.Join(dir, "work"), d.BaseRev, d.tip())
	if err != nil {
		http.Error(w, "diff unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, frag)
}

// handleShapeDraftPage serves the draft's tip exactly as the app would be
// served, for its owner only, over a private copy of their data — the
// preview path — with the Genie component in draft mode on every page.
func (s *server) handleShapeDraftPage(w http.ResponseWriter, r *http.Request) {
	a, principal, d, dir, ok := s.draftFor(w, r)
	if !ok {
		return
	}
	repo, err := gogit.PlainOpen(filepath.Join(dir, "work"))
	if err != nil {
		http.Error(w, "draft unavailable", http.StatusInternalServerError)
		return
	}
	commit, err := repo.CommitObject(plumbing.NewHash(d.tip()))
	if err != nil {
		http.Error(w, "draft unavailable", http.StatusInternalServerError)
		return
	}
	data, err := a.previewData(principal, draftDirName)
	if err != nil {
		http.Error(w, "draft data unavailable", http.StatusServiceUnavailable)
		return
	}
	prefix := "/shape/" + a.name + "/" + draftDirName
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if rest == "" {
		http.Redirect(w, r, coopauth.PrefixPath(r, prefix+"/"), http.StatusFound)
		return
	}
	r2 := withDraft(r.Clone(r.Context()))
	r2.URL.Path = rest
	if a.name == consoleApp {
		s.serveConsolePreview(w, r2, commit, prefix)
		return
	}
	a.serveTree(w, r2, commit, mountRel(r2), principal, data)
}

// draftRoutes registers the edit session beside the candidate routes;
// every route starts at shapeCaller, and the scope rules treat
// /shape/<app>/draft/… as reaching <app> like any other shape route.
func (s *server) draftRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/shape/{app}/"+draftDirName+"/", s.handleShapeDraftPage)
	mux.HandleFunc("GET /api/shape/{app}/draft", s.handleShapeDraftState)
	mux.HandleFunc("POST /api/shape/{app}/draft", s.handleShapeDraftStart)
	mux.HandleFunc("POST /api/shape/{app}/draft/wishes", s.handleShapeDraftWish)
	mux.HandleFunc("POST /api/shape/{app}/draft/undo", s.handleShapeDraftUndo)
	mux.HandleFunc("POST /api/shape/{app}/draft/redo", s.handleShapeDraftRedo)
	mux.HandleFunc("POST /api/shape/{app}/draft/save", s.handleShapeDraftSave)
	mux.HandleFunc("POST /api/shape/{app}/draft/cancel", s.handleShapeDraftCancel)
	mux.HandleFunc("GET /api/shape/{app}/draft/diff", s.handleShapeDraftDiff)
}
