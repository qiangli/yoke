package kb

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kblife"
)

func TestSupersedeRecordsValidityInterval(t *testing.T) {
	dir := t.TempDir()
	mustRun(t, dir, "add", "--title", "use flag x", "--description", "WHEN building")
	before, err := os.ReadFile(Open(dir).PagePath("use-flag-x"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte("valid_")) {
		t.Fatalf("an unsuperseded page must carry no interval on disk:\n%s", before)
	}
	mustRun(t, dir, "supersede", "use-flag-x", "--title", "use flag y", "--description", "WHEN building")
	store := Open(dir)
	old, _ := store.Load("use-flag-x")
	neu, _ := store.Load("use-flag-y")
	if old.ValidTo == "" || old.ValidTo != neu.ValidFrom {
		t.Fatalf("old.valid_to %q must equal new.valid_from %q", old.ValidTo, neu.ValidFrom)
	}
	if old.ValidFrom != "" || neu.ValidTo != "" {
		t.Fatalf("open bounds must stay open: old.from=%q new.to=%q", old.ValidFrom, neu.ValidTo)
	}
	// Superseding an already-closed page is refused and writes nothing.
	if _, err := run(t, dir, "", "supersede", "use-flag-x", "--title", "use flag z", "--description", "d"); err == nil {
		t.Fatal("re-superseding a closed page should fail")
	}
	if _, err := store.Load("use-flag-z"); err == nil {
		t.Fatal("refused supersede wrote the replacement")
	}
}

func TestForgetHidesAndKeepsAudit(t *testing.T) {
	dir := t.TempDir()
	mustRun(t, dir, "add", "--title", "rotate the widget token", "--description", "WHEN widget auth fails")
	mustRun(t, dir, "add", "--title", "widget cache warmup", "--description", "WHEN widget is slow")

	if _, err := run(t, dir, "", "forget", "rotate-the-widget-token"); err == nil {
		t.Fatal("forget without --reason should fail")
	}
	out := mustRun(t, dir, "forget", "rotate-the-widget-token", "--reason", "token no longer exists", "--json")
	var tomb kblife.Tombstone
	if err := json.Unmarshal([]byte(out), &tomb); err != nil {
		t.Fatalf("--json: %v\n%s", err, out)
	}
	if tomb.Ref != "kb:rotate-the-widget-token" || tomb.Reason != "token no longer exists" || tomb.By == "" || tomb.At.IsZero() {
		t.Fatalf("tombstone = %+v", tomb)
	}

	store := Open(dir)
	if _, err := os.Stat(store.PagePath("rotate-the-widget-token")); err != nil {
		t.Fatalf("forget must never delete the page file: %v", err)
	}
	for _, args := range [][]string{{"search", "widget"}, {"search", "--all", "widget"}} {
		if got := mustRun(t, dir, args...); strings.Contains(got, "rotate-the-widget-token") {
			t.Fatalf("kb %v returned a forgotten page:\n%s", args, got)
		}
	}
	idx, _ := os.ReadFile(store.indexPath())
	if strings.Contains(string(idx), "rotate-the-widget-token") {
		t.Fatalf("index lists a forgotten page:\n%s", idx)
	}

	// show still opens it: outcome found, plus the lifecycle field.
	var env ShowEnvelope
	if err := json.Unmarshal([]byte(mustRun(t, dir, "show", "--json", "rotate-the-widget-token")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Outcome != string(OutcomeFound) || env.Lifecycle == nil || env.Lifecycle.State != kblife.StateForgotten ||
		env.Lifecycle.Tombstone == nil || env.Lifecycle.Tombstone.Reason != "token no longer exists" {
		t.Fatalf("show envelope = %+v (lifecycle %+v)", env, env.Lifecycle)
	}
	// A valid page's envelope carries no lifecycle field.
	if live := mustRun(t, dir, "show", "--json", "widget-cache-warmup"); strings.Contains(live, "lifecycle") {
		t.Fatalf("valid page envelope grew a lifecycle field:\n%s", live)
	}

	// The audit trail keeps who, when and why.
	log := mustRun(t, dir, "log")
	var rec forgetRecord
	found := false
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if json.Unmarshal([]byte(l), &rec) == nil && rec.Op == "forget" {
			found = true
			break
		}
	}
	if !found || rec.Reason != tomb.Reason || rec.By != tomb.By || !rec.At.Equal(tomb.At) || rec.Slug != "rotate-the-widget-token" {
		t.Fatalf("journal forget record = %+v\n%s", rec, log)
	}
	if _, err := run(t, dir, "", "forget", "rotate-the-widget-token", "--reason", "again"); err == nil {
		t.Fatal("forgetting twice should fail")
	}
}

func TestVisibleAtResolvesAsOf(t *testing.T) {
	cut := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	old := &Page{Slug: "old", Status: StatusSuperseded, SupersededBy: "new", ValidTo: cut.Format(time.RFC3339)}
	neu := &Page{Slug: "new", Status: StatusCandidate, ValidFrom: cut.Format(time.RFC3339)}
	gone := &Page{Slug: "gone", Status: StatusValidated}
	tombs := map[string]kblife.Tombstone{"gone": {Ref: "kb:gone", Reason: "r", By: "b", At: cut}}
	slugs := func(ps []*Page) string {
		var s []string
		for _, p := range ps {
			s = append(s, p.Slug)
		}
		return strings.Join(s, ",")
	}
	if got := slugs(VisibleAt([]*Page{old, neu, gone}, tombs, cut.Add(-time.Hour))); got != "old,gone" {
		t.Fatalf("before cut = %s", got)
	}
	if got := slugs(VisibleAt([]*Page{old, neu, gone}, tombs, cut.Add(time.Hour))); got != "new" {
		t.Fatalf("after cut = %s", got)
	}
}
