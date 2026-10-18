package recall

import (
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
)

var lifeCut = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// supersededPair writes the on-disk result of `kb supersede` at lifeCut.
func supersededPair(t *testing.T) *kb.Store {
	t.Helper()
	old := page("widget-old", "widget retry policy", "WHEN widget calls fail retry 3 times")
	old.Status, old.SupersededBy, old.ValidTo = kb.StatusSuperseded, "widget-new", lifeCut.Format(time.RFC3339)
	neu := page("widget-new", "widget retry policy revised", "WHEN widget calls fail retry with backoff")
	neu.Supersedes, neu.ValidFrom = "widget-old", lifeCut.Format(time.RFC3339)
	return kbWith(t, t.TempDir(), old, neu)
}

func ids(hits []Hit) string {
	var s []string
	for _, h := range hits {
		s = append(s, h.ID)
	}
	return strings.Join(s, ",")
}

func blockRefs(blocks []ContextBlock) string {
	var s []string
	for _, b := range blocks {
		s = append(s, b.Ref)
	}
	return strings.Join(s, ",")
}

func TestRecallAsOfSupersede(t *testing.T) {
	store := supersededPair(t)
	before := Recall(Query{Text: "widget retry", K: 5, AsOf: lifeCut.Add(-time.Hour)}, HostRing{Store: store})
	if got := ids(before.Hits); got != "kb:widget-old" {
		t.Fatalf("as-of before supersede = %q, want only the old page", got)
	}
	if before.Hits[0].ValidTo != lifeCut.Format(time.RFC3339) {
		t.Fatalf("old hit valid_to = %q", before.Hits[0].ValidTo)
	}
	after := Recall(Query{Text: "widget retry", K: 5, AsOf: lifeCut.Add(time.Hour)}, HostRing{Store: store})
	if got := ids(after.Hits); got != "kb:widget-new" {
		t.Fatalf("as-of after supersede = %q, want only the replacement", got)
	}
	if after.Hits[0].ValidFrom != lifeCut.Format(time.RFC3339) {
		t.Fatalf("replacement valid_from = %q", after.Hits[0].ValidFrom)
	}
	// Default as-of is now: the replacement.
	if got := ids(Recall(Query{Text: "widget retry", K: 5}, HostRing{Store: store}).Hits); got != "kb:widget-new" {
		t.Fatalf("default as-of = %q", got)
	}
}

func TestContextAsOfSupersedePopulatesValidTo(t *testing.T) {
	store := supersededPair(t)
	before := Context(Query{Text: "widget retry", K: 5, AsOf: lifeCut.Add(-time.Hour)}, HostRing{Store: store})
	if got := blockRefs(before.Blocks); got != "kb:widget-old" {
		t.Fatalf("context before = %q", got)
	}
	if before.Blocks[0].ValidTo != lifeCut.Format(time.RFC3339) {
		t.Fatalf("context block valid_to = %q", before.Blocks[0].ValidTo)
	}
	after := Context(Query{Text: "widget retry", K: 5, AsOf: lifeCut.Add(time.Hour)}, HostRing{Store: store})
	if got := blockRefs(after.Blocks); got != "kb:widget-new" {
		t.Fatalf("context after = %q", got)
	}
	if after.Blocks[0].ValidTo != "" {
		t.Fatalf("open replacement emitted valid_to %q", after.Blocks[0].ValidTo)
	}
}

func TestForgetHidesFromRecallAndContext(t *testing.T) {
	store := kbWith(t, t.TempDir(),
		page("widget-token", "widget token rotation", "WHEN widget auth fails"),
		page("widget-cache", "widget cache warmup", "WHEN widget is slow"))
	p, err := store.Load("widget-token")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if _, err := store.Forget(p, "obsolete", at); err != nil {
		t.Fatal(err)
	}
	if got := ids(Recall(Query{Text: "widget", K: 5}, HostRing{Store: store}).Hits); strings.Contains(got, "widget-token") || got == "" {
		t.Fatalf("recall after forget = %q", got)
	}
	if got := blockRefs(Context(Query{Text: "widget", K: 5}, HostRing{Store: store}).Blocks); strings.Contains(got, "widget-token") || got == "" {
		t.Fatalf("context after forget = %q", got)
	}
	// History still answers: as-of before the forget the page was known.
	if got := ids(Recall(Query{Text: "widget token", K: 5, AsOf: at.Add(-time.Minute)}, HostRing{Store: store}).Hits); !strings.Contains(got, "widget-token") {
		t.Fatalf("as-of before forget = %q", got)
	}
}

func TestParseAsOf(t *testing.T) {
	if got, err := ParseAsOf("2026-05-01"); err != nil || !got.Equal(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("date: %v %v", got, err)
	}
	if got, err := ParseAsOf("2026-05-01T12:00:00Z"); err != nil || !got.Equal(lifeCut) {
		t.Fatalf("rfc3339: %v %v", got, err)
	}
	if _, err := ParseAsOf("yesterday"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestRecallCmdRejectsBadAsOf(t *testing.T) {
	cmd := NewRecallCmd()
	cmd.SetArgs([]string{"--as-of", "nope", "widget"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "as-of") {
		t.Fatalf("err = %v", err)
	}
}
