// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	shapecore "github.com/qiangli/yoke/pkg/shape"
	"github.com/qiangli/yoke/pkg/shape/appserver"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

type ledgerCaptureShaper struct {
	mu      sync.Mutex
	tasks   []string
	dirs    []string
	ledgers [][]byte
}

func (f *ledgerCaptureShaper) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	raw, _ := os.ReadFile(filepath.Join(req.Dir, filepath.FromSlash(shapecore.WorkLedgerPath)))
	f.mu.Lock()
	f.tasks = append(f.tasks, req.Task)
	f.dirs = append(f.dirs, req.Dir)
	f.ledgers = append(f.ledgers, raw)
	n := len(f.tasks)
	f.mu.Unlock()
	if err := os.WriteFile(filepath.Join(req.Dir, "api", "list.md"), []byte(fmt.Sprintf("```bsh\nprintf 'ok %d\\n'\n```\n", n)), 0o600); err != nil {
		return DelegateResult{}, err
	}
	return DelegateResult{
		Agent:  "stub:ledger",
		Output: "COVERED:\n- updated\nNOT COVERED:\n- (none)\nWHY:\n- tested\nEXPECT:\n- passes\n",
	}, nil
}

func seedLedgerEntry(t *testing.T, home, app, base, owner, member string) {
	t.Helper()
	for _, d := range []shape.AutoDecision{
		{App: app, Event: "trigger", Result: "wish", Source: "human", Problem: "todo:T-99", Principal: member, Line: owner, Base: base, Detail: "0123456789abcdef add footer"},
		{App: app, Event: "activation", Result: "activated", Source: "human", Problem: "todo:T-99", Principal: member, Line: owner, Base: base, Rev: base, Agent: "stub", Why: "owner " + owner + " path /home/op/secret", Detail: "0123456789abcdef " + base},
	} {
		if err := shape.LogAuto(home, d); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNotInCommit(t *testing.T, workDir, rev string) {
	t.Helper()
	repo, err := gogit.PlainOpen(workDir)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(rev))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.FindEntry(shapecore.WorkLedgerPath); err == nil {
		t.Fatalf("commit %s contains %s", rev, shapecore.WorkLedgerPath)
	}
	if _, err := tree.FindEntry(".git"); err == nil {
		t.Fatalf("commit %s contains .git", rev)
	}
}

func TestConsoleWishBriefIncludesLedgerAndWorkCloneFile(t *testing.T) {
	const ownerPrincipal = "cloud:o@example.test"
	const memberPrincipal = "cloud:c@example.test"

	home := t.TempDir()
	workRoot := filepath.Join(t.TempDir(), "work")
	source := newNotesSource(t)
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	base := head.Hash().String()

	shaper := &ledgerCaptureShaper{}
	scopes := &appserver.Scopes{Shape: &appserver.Audience{Emails: []string{"o@example.test", "c@example.test"}}}
	s := &server{opts: Options{
		Ctx: context.Background(),
		Shape: ShapeOptions{
			Home: home, WorkRoot: workRoot, Owner: ownerPrincipal, Scopes: scopes,
			Delegate: shaper, Verifier: &fakeVerifier{}, Activator: &fakeActivator{home: home},
		},
	}}
	a := &shapedApp{name: "notes", repo: source, owner: ownerPrincipal, scopes: scopes, opts: s.opts.Shape}

	// Empty log: no ledger block in the brief.
	cEmpty, err := s.startCandidate(a, ownerPrincipal, wish{Request: "wish empty"})
	if err != nil {
		t.Fatal(err)
	}
	s.shapes.wg.Wait()
	if strings.Contains(shaper.tasks[0], "<<<LEDGER") || strings.Contains(shaper.tasks[0], "Earlier attempts") {
		t.Fatalf("empty log rendered ledger:\n%s", shaper.tasks[0])
	}
	_ = cEmpty

	// Seed decision log and run owner + member console wishes.
	seedLedgerEntry(t, home, "notes", base, ownerPrincipal, memberPrincipal)

	cOwner, err := s.startCandidate(a, ownerPrincipal, wish{Request: "owner wish"})
	if err != nil {
		t.Fatal(err)
	}
	s.shapes.wg.Wait()
	ownerLoaded, _, err := a.loadCandidate(ownerPrincipal, cOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shaper.tasks[1], "<<<LEDGER\n") || !strings.Contains(shaper.tasks[1], "\nLEDGER>>>\n") || !strings.Contains(shaper.tasks[1], "todo:T-99") {
		t.Fatalf("owner console brief missing fenced ledger:\n%s", shaper.tasks[1])
	}
	var gotLedger shapecore.Ledger
	if err := json.Unmarshal(shaper.ledgers[1], &gotLedger); err != nil {
		t.Fatalf("bad owner work ledger %s: %v", shaper.ledgers[1], err)
	}
	foundT99 := false
	for _, p := range gotLedger.Problems {
		if p.Key == "todo:T-99" {
			foundT99 = true
		}
	}
	if !foundT99 {
		t.Fatalf("owner work ledger missing todo:T-99: %s", shaper.ledgers[1])
	}
	assertNotInCommit(t, shaper.dirs[1], ownerLoaded.Rev)

	if _, err := s.startCandidate(a, memberPrincipal, wish{Request: "member wish"}); err != nil {
		t.Fatal(err)
	}
	s.shapes.wg.Wait()
	if !strings.Contains(shaper.tasks[2], "<<<LEDGER\n") || !strings.Contains(shaper.tasks[2], "add footer") {
		t.Fatalf("member console brief missing ledger:\n%s", shaper.tasks[2])
	}
	for _, leak := range []string{"todo:T-99", "T-99", ownerPrincipal, "/home/op"} {
		if strings.Contains(shaper.tasks[2], leak) {
			t.Errorf("member console brief leaks %q:\n%s", leak, shaper.tasks[2])
		}
		if strings.Contains(string(shaper.ledgers[2]), leak) {
			t.Errorf("member work ledger leaks %q: %s", leak, shaper.ledgers[2])
		}
	}
}

func TestDraftStepBriefIncludesLedgerAndWorkCloneFile(t *testing.T) {
	const ownerPrincipal = "cloud:o@example.test"
	const memberPrincipal = "cloud:c@example.test"

	home := t.TempDir()
	workRoot := filepath.Join(t.TempDir(), "work")
	source := newNotesSource(t)
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	base := head.Hash().String()

	shaper := &ledgerCaptureShaper{}
	scopes := &appserver.Scopes{Shape: &appserver.Audience{Emails: []string{"o@example.test", "c@example.test"}}}
	s := &server{opts: Options{
		Ctx: context.Background(),
		Shape: ShapeOptions{
			Home: home, WorkRoot: workRoot, Owner: ownerPrincipal, Scopes: scopes,
			Delegate: shaper, Verifier: &fakeVerifier{}, Activator: &fakeActivator{home: home},
		},
	}}
	a := &shapedApp{name: "notes", repo: source, owner: ownerPrincipal, scopes: scopes, opts: s.opts.Shape}

	// Empty log: no ledger block.
	if _, _, err := s.startDraft(context.Background(), a, ownerPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addDraftWish(a, ownerPrincipal, wish{Request: "step empty"}); err != nil {
		t.Fatal(err)
	}
	s.shapes.wg.Wait()
	if strings.Contains(shaper.tasks[0], "<<<LEDGER") || strings.Contains(shaper.tasks[0], "Earlier attempts") {
		t.Fatalf("empty log rendered ledger in draft:\n%s", shaper.tasks[0])
	}

	// Seed decision log and run owner + member draft steps.
	seedLedgerEntry(t, home, "notes", base, ownerPrincipal, memberPrincipal)
	if _, err := s.addDraftWish(a, ownerPrincipal, wish{Request: "owner step"}); err != nil {
		t.Fatal(err)
	}
	s.shapes.wg.Wait()
	d, _, err := a.loadDraft(ownerPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shaper.tasks[1], "<<<LEDGER\n") || !strings.Contains(shaper.tasks[1], "\nLEDGER>>>\n") || !strings.Contains(shaper.tasks[1], "todo:T-99") {
		t.Fatalf("owner draft brief missing fenced ledger:\n%s", shaper.tasks[1])
	}
	if len(shaper.ledgers[1]) == 0 {
		t.Fatal("draft work clone missing .git/shape-ledger.json")
	}
	assertNotInCommit(t, shaper.dirs[1], d.Steps[1].Rev)

	if _, _, err := s.startDraft(context.Background(), a, memberPrincipal); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addDraftWish(a, memberPrincipal, wish{Request: "member step"}); err != nil {
		t.Fatal(err)
	}
	s.shapes.wg.Wait()
	if !strings.Contains(shaper.tasks[2], "<<<LEDGER\n") || !strings.Contains(shaper.tasks[2], "add footer") {
		t.Fatalf("member draft brief missing ledger:\n%s", shaper.tasks[2])
	}
	for _, leak := range []string{"todo:T-99", "T-99", ownerPrincipal, "/home/op"} {
		if strings.Contains(shaper.tasks[2], leak) {
			t.Errorf("member draft brief leaks %q:\n%s", leak, shaper.tasks[2])
		}
		if strings.Contains(string(shaper.ledgers[2]), leak) {
			t.Errorf("member draft work ledger leaks %q: %s", leak, shaper.ledgers[2])
		}
	}
}

func TestRepairBriefIncludesLedgerAndWorkCloneFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	const ownerPrincipal = "cloud:o@example.test"
	const memberPrincipal = "cloud:c@example.test"

	app, _, _, base := repairFixture(t, "fix")
	shaper := &ledgerCaptureShaper{}
	app.opts.Delegate = shaper
	app.opts.Verifier = productionVerifier{home: app.opts.Home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}

	in := Incident{ID: "i1", App: "notes", Kind: IncidentAPI, Path: "api/list", BaseRev: base}
	if _, _, reason, _, _ := app.repairAttempt(context.Background(), "device:alice", in, ""); reason != "" {
		t.Fatalf("empty log repair failed: %s", reason)
	}
	if strings.Contains(shaper.tasks[0], "<<<LEDGER") || strings.Contains(shaper.tasks[0], "Earlier attempts") {
		t.Fatalf("empty log rendered ledger in repair:\n%s", shaper.tasks[0])
	}

	app.owner = ownerPrincipal
	app.opts.Activator = &fakeActivator{home: app.opts.Home}
	seedLedgerEntry(t, app.opts.Home, "notes", base, ownerPrincipal, memberPrincipal)
	if st, err := shape.OpenAt(app.opts.Home, "notes", memberPrincipal); err == nil {
		_ = st.Write(shape.State{ActiveRevision: base})
	}
	c, _, reason, _, _ := app.repairAttempt(context.Background(), memberPrincipal, in, "first try failed")
	if reason != "" && !strings.Contains(reason, "patch release") {
		t.Fatalf("member repair failed: %s", reason)
	}
	if !strings.Contains(shaper.tasks[1], "<<<LEDGER\n") || !strings.Contains(shaper.tasks[1], "\nLEDGER>>>\n") || !strings.HasSuffix(shaper.tasks[1], "\nINCIDENT>>>\n") {
		t.Fatalf("repair brief missing fenced ledger or incident suffix:\n%s", shaper.tasks[1])
	}
	if !strings.Contains(shaper.tasks[1], "add footer") {
		t.Fatalf("repair brief missing entry:\n%s", shaper.tasks[1])
	}
	if len(shaper.ledgers[1]) == 0 {
		t.Fatal("repair work clone missing .git/shape-ledger.json")
	}
	for _, leak := range []string{"todo:T-99", "T-99", ownerPrincipal, "/home/op"} {
		if strings.Contains(shaper.tasks[1], leak) {
			t.Errorf("member repair brief leaks %q:\n%s", leak, shaper.tasks[1])
		}
		if strings.Contains(string(shaper.ledgers[1]), leak) {
			t.Errorf("member repair work ledger leaks %q: %s", leak, shaper.ledgers[1])
		}
	}
	assertNotInCommit(t, shaper.dirs[1], c.Rev)
	// The repair activates on the line its evidence was sealed for, with the
	// member as author when that line is someone else's.
	svc := app.candidateService()
	line := svc.QueuePrincipal(svc.ActivationPrincipal(memberPrincipal))
	if calls := app.opts.Activator.(*fakeActivator).calls; len(calls) > 0 {
		got := calls[len(calls)-1]
		wantAuthor := ""
		if line != memberPrincipal {
			wantAuthor = memberPrincipal
		}
		if got.Principal != line || got.Author != wantAuthor {
			t.Fatalf("member repair activated principal=%q author=%q, want line %q author %q", got.Principal, got.Author, line, wantAuthor)
		}
	}
}
