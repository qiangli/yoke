// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/policy/audit"
)

func guardErr(t *testing.T, err error) *Conflict {
	t.Helper()
	var c *Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want *Conflict", err)
	}
	return c
}

func TestGuardRefusesNonHolderAdmitsHolder(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "yoke"}, Members: []string{"/w/yoke"}, Holder: agentA(), Intent: "gate"}); err != nil {
		t.Fatal(err)
	}
	// Same-key use, and a path use under the repo (same domain, path rule).
	for _, u := range []Use{
		{Kind: "repo", Name: "yoke"},
		{Kind: "path", Name: "/w/yoke/pkg/x.go"},
		{Kind: "path", Name: "x", Member: "/w/yoke/pkg/x.go"},
		{Kind: "repo", Name: "other", Member: "/w/yoke"},
	} {
		c := guardErr(t, Guard(ctx, agentB(), u))
		if c.Claim.Holder.Name != "claude-a" {
			t.Errorf("%+v: holder = %q", u, c.Claim.Holder.Name)
		}
		if err := Guard(ctx, agentA(), u); err != nil {
			t.Errorf("%+v: the holder was refused: %v", u, err)
		}
	}
	// Outside the claim, another domain, or no uses at all: admitted.
	for _, u := range []Use{
		{Kind: "path", Name: "/w/elsewhere"},
		{Kind: "repo", Name: "yoke", Member: "/w/else"},
		{Kind: "name", Name: "yoke"},
		{Kind: "sprint", Name: "/w/yoke"},
	} {
		if err := Guard(ctx, agentB(), u); err != nil {
			t.Errorf("%+v: refused: %v", u, err)
		}
	}
	if err := Guard(ctx, agentB()); err != nil {
		t.Fatal(err)
	}
	// One blocked use among several blocks the call.
	if err := Guard(ctx, agentB(), Use{Kind: "path", Name: "/free"}, Use{Kind: "repo", Name: "yoke"}); err == nil {
		t.Fatal("a blocked use among several was admitted")
	}
}

func TestGuardSeesLegacyProjectClaims(t *testing.T) {
	dir := ledger(t)
	if _, err := Acquire(dir, []string{"/w/bashy", "/w/sh"}, agentA(), "", false); err != nil {
		t.Fatal(err)
	}
	if err := Guard(context.Background(), agentB(), Use{Kind: "path", Name: "/w/sh/interp"}); err == nil {
		t.Fatal("legacy project claim not enforced by Guard")
	}
	if err := Guard(context.Background(), agentA(), Use{Kind: "path", Name: "/w/sh/interp"}); err != nil {
		t.Fatal(err)
	}
}

func TestGuardIgnoresAnnounceButAcquireDoesNot(t *testing.T) {
	ledger(t)
	ctx := context.Background()
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"repo", "yoke"}, Members: []string{"/w/yoke"}, Holder: agentA(), Mode: ModeAnnounce}); err != nil {
		t.Fatal(err)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "repo", Name: "yoke"}, Use{Kind: "path", Name: "/w/yoke/a"}); err != nil {
		t.Fatalf("an announce blocked Guard: %v", err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"path", "/w/yoke/a"}, Holder: agentB()}); !isConflict(err) {
		t.Fatalf("an announce did not conflict on Acquire: %v", err)
	}
}

func TestGuardLivenessRules(t *testing.T) {
	dir := ledger(t)
	ctx := context.Background()
	g, _ := AcquireRef(ctx, req("name:gl", agentA()))
	u := Use{Kind: "name", Name: "gl"}

	unknown := *g.Claim
	unknown.Heartbeat = time.Time{}
	if err := writeClaim(resourceClaimPath(dir, "gl"), &unknown); err != nil {
		t.Fatal(err)
	}
	if err := Guard(ctx, agentB(), u); err == nil {
		t.Fatal("an UNKNOWN claim was treated as free by Guard")
	}

	lapsed := *g.Claim
	backdate(t, resourceClaimPath(dir, "gl"), &lapsed)
	if err := Guard(ctx, agentB(), u); err != nil {
		t.Fatalf("a lapsed claim blocked Guard: %v", err)
	}
}

func TestGuardCustomBackend(t *testing.T) {
	ledger(t)
	mb := &memBackend{m: map[string]*Claim{}}
	RegisterKind(Kind{Name: "test-guardmem", Match: MatchName})
	RegisterBackend("test-guardmem", mb)
	t.Cleanup(func() { RegisterBackend("test-guardmem", nil) })
	ctx := context.Background()
	if _, err := AcquireRef(ctx, req("test-guardmem:k", agentA())); err != nil {
		t.Fatal(err)
	}
	if err := Guard(ctx, agentB(), Use{Kind: "test-guardmem", Name: "k"}); err == nil {
		t.Fatal("Guard ignored a custom-backend claim")
	}
	if err := Guard(ctx, agentA(), Use{Kind: "test-guardmem", Name: "k"}); err != nil {
		t.Fatal(err)
	}
}

func fixedConflict() *Conflict {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &Conflict{
		Claim: &Claim{
			SchemaVersion: SchemaVersion, Kind: "repo", Resource: "yoke", Mode: ModeLease, Epoch: 3,
			Holder: agentA(), Intent: "running the gate", AcquiredAt: at, Heartbeat: at,
		},
		now: at.Add(time.Minute),
	}
}

const conflictGolden = `{
  "schema_version": "bashy-claim-conflict-v1",
  "holder": "claude-a",
  "resource": "repo:yoke",
  "kind": "repo",
  "intent": "running the gate",
  "since": "2026-01-02T03:04:05Z",
  "liveness": "live",
  "contacts": [
    "bashy ping claude-a \"\u003cwhy you need it\u003e\"",
    "bashy claim request repo:yoke -m \"\u003creason\u003e\"",
    "bashy meet dm claude-a",
    "bashy claim repo:yoke --wait 30m"
  ]
}`

func TestConflictJSONGolden(t *testing.T) {
	got, err := fixedConflict().JSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != conflictGolden {
		t.Fatalf("conflict JSON drifted.\n got: %s\nwant: %s", got, conflictGolden)
	}
	var back map[string]any
	if err := json.Unmarshal(got, &back); err != nil || back["liveness"] != "live" {
		t.Fatalf("not valid JSON: %v", err)
	}
}

func TestConflictErrorNamesEverythingAndContacts(t *testing.T) {
	c := fixedConflict()
	msg := c.Error()
	for _, want := range []string{"claude-a", "repo:yoke", "running the gate", "since", "live", "on h"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() lacks %q:\n%s", want, msg)
		}
	}
	for _, line := range c.Contacts() {
		if !strings.Contains(msg, line) {
			t.Errorf("Error() lacks contact %q", line)
		}
	}
	if got := c.Contacts(); len(got) != 4 || !strings.HasPrefix(got[0], "bashy ping claude-a ") ||
		!strings.HasPrefix(got[1], "bashy claim request repo:yoke -m ") ||
		got[2] != "bashy meet dm claude-a" || got[3] != "bashy claim repo:yoke --wait 30m" {
		t.Fatalf("contacts = %q", got)
	}
}

func TestConflictHolderFallbacks(t *testing.T) {
	c := &Conflict{Claim: &Claim{Resource: "x", Holder: agentA()}}
	c.Claim.Holder.Name = ""
	if !strings.Contains(c.Contacts()[0], "ep-aaa") {
		t.Fatalf("contacts without a name should address the episode: %q", c.Contacts())
	}
	c.Claim.Holder.Episode = ""
	if !strings.Contains(c.Contacts()[0], "<holder>") || !strings.Contains(c.Error(), "another agent") {
		t.Fatal("anonymous holder not handled")
	}
}

func readAudit(t *testing.T, path string) []audit.Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if res := audit.Verify(bytes.NewReader(b)); !res.OK {
		t.Fatalf("audit chain broken: %+v", res)
	}
	var out []audit.Record
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestForceThatDisplacesWritesAudit(t *testing.T) {
	dir := ledger(t)
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("BASHY_AUDIT", logPath)
	ctx := context.Background()

	// Unforced and uncontested acquisitions leave no record.
	if _, err := AcquireRef(ctx, req("name:fa", agentA())); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "fb"}, Holder: agentB(), Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := readAudit(t, logPath); len(got) != 0 {
		t.Fatalf("a force that displaced nobody was audited: %+v", got)
	}

	// Same-key takeover.
	if _, err := AcquireRef(ctx, Request{Ref: Ref{"name", "fa"}, Holder: agentB(), Force: true, Intent: "urgent"}); err != nil {
		t.Fatal(err)
	}
	// Legacy project claim, via the Enforce-shaped wrapper.
	if _, err := Acquire(dir, []string{"/w/p"}, agentA(), "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, []string{"/w/p/sub"}, agentC(), "", true); err != nil {
		t.Fatal(err)
	}

	recs := readAudit(t, logPath)
	if len(recs) != 2 {
		t.Fatalf("audit records = %d, want 2: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Action != "claim.force" || r.Time == "" || r.Actor.Agent == "" {
		t.Fatalf("record = %+v", r)
	}
	joined := strings.Join(r.Argv, " ")
	for _, want := range []string{"name:fa", "claude-a@name:fa", "intent=urgent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q lacks %q", joined, want)
		}
	}
	if !strings.Contains(strings.Join(recs[1].Argv, " "), "claude-a@repo:p") {
		t.Errorf("legacy force argv = %v", recs[1].Argv)
	}
}

func TestForceOverAttachedIsStillRefused(t *testing.T) {
	ledger(t)
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	t.Setenv("BASHY_AUDIT", logPath)
	_, l, err := AcquireAttachedRef(context.Background(), Request{Ref: Ref{"name", "kernel"}, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := AcquireRef(context.Background(), Request{Ref: Ref{"name", "kernel"}, Holder: agentB(), Force: true}); !isConflict(err) {
		t.Fatalf("force overrode a kernel hold: %v", err)
	}
	if got := readAudit(t, logPath); len(got) != 0 {
		t.Fatalf("a refused force was audited as an override: %+v", got)
	}
}
