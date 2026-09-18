// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

func telemetryHint(target, service string) string {
	return "bashy: telemetry on → " + target + " (service=" + service + ")\n"
}

func TestTelemetryHintClassifierIsClosed(t *testing.T) {
	good := telemetryHint("$HOME/.agents/otel/spool/spans.jsonl", "bashy")
	if !IsTelemetryHint([]byte(good)) {
		t.Fatal("canonical startup notice was not classified")
	}
	disabled := "bashy: telemetry disabled — spool: unavailable\n"
	if !IsTelemetryHint([]byte(disabled)) {
		t.Fatal("canonical disabled notice was not classified")
	}
	for _, line := range []string{
		"telemetry is on\n",
		"bashy: telemetry disabled — \n",
		"bashy: telemetry disabled - spool: unavailable\n",
		"bashy: telemetry on -> /tmp/spans (service=bashy)\n",
		"prefix " + good,
		strings.TrimSuffix(good, "\n") + " trailing\n",
		"bashy: telemetry on → /tmp/spans (service=not a service)\n",
		`{"message":"bashy: telemetry on → /tmp/spans (service=bashy)"}` + "\n",
	} {
		if IsTelemetryHint([]byte(line)) {
			t.Errorf("classified non-hint %q", line)
		}
	}
}

func TestTelemetryHintDeduperPreservesNonTelemetryAndNearDuplicates(t *testing.T) {
	a := telemetryHint("http://127.0.0.1:4317", "bashy")
	b := telemetryHint("http://127.0.0.1:4318", "bashy")
	ordinary := "PASS package/example\n\nPASS package/example\n"
	in := []byte(a + b + ordinary + "payload line one\npayload line two")
	got, suppressed, _ := deduplicateTelemetryHints(in)
	if suppressed != 0 || !bytes.Equal(got, in) {
		t.Fatalf("near/nontelemetry lines changed: suppressed=%d\ngot=%q\nwant=%q", suppressed, got, in)
	}
}

func TestTelemetryHintDeduperKeepsFirstOfEachExactValue(t *testing.T) {
	a := telemetryHint("http://127.0.0.1:4317", "bashy")
	b := telemetryHint("http://127.0.0.1:4318", "bashy")
	disabled := "bashy: telemetry disabled — spool: unavailable\n"
	ordinary := "same ordinary line\n"
	in := []byte(a + b + disabled + ordinary + a + disabled + ordinary + b)
	got, suppressed, _ := deduplicateTelemetryHints(in)
	want := a + b + disabled + ordinary + ordinary
	if suppressed != 3 || string(got) != want {
		t.Fatalf("suppressed=%d text=%q, want 3 and %q", suppressed, got, want)
	}
}

func TestTelemetryHintDeduperIsChunkBoundaryIndependent(t *testing.T) {
	hint := telemetryHint("$HOME/.agents/otel/spool/spans.jsonl", "bashy")
	in := []byte("before\n" + hint + "middle\n" + hint + hint + "after")
	want, wantCount, wantBytes := deduplicateTelemetryHints(in)
	for step := 1; step <= len(in)+1; step++ {
		var d telemetryHintDeduper
		for at := 0; at < len(in); at += step {
			d.write(in[at:min(at+step, len(in))])
		}
		got, count, omitted := d.finish()
		if !bytes.Equal(got, want) || count != wantCount || omitted != wantBytes {
			t.Fatalf("chunk size %d changed result: count=%d bytes=%d text=%q", step, count, omitted, got)
		}
	}
}

func TestTelemetryHintDeduperBoundsClassifierStateAndFailsOpen(t *testing.T) {
	var d telemetryHintDeduper
	for i := 0; i < maxRememberedHints+20; i++ {
		d.write([]byte(telemetryHint(fmt.Sprintf("http://collector/%03d", i), "bashy")))
		if len(d.seen) > maxRememberedHints || len(d.pending) > maxTelemetryHintBytes+2 {
			t.Fatalf("unbounded state: seen=%d pending=%d", len(d.seen), len(d.pending))
		}
	}
	// This line was encountered after the fixed registry filled, so its exact
	// repeat must be retained rather than guessed duplicate.
	unremembered := telemetryHint("http://collector/070", "bashy")
	d.write([]byte(unremembered))
	got, suppressed, _ := d.finish()
	if suppressed != 0 || strings.Count(string(got), unremembered) != 2 {
		t.Fatalf("full registry did not fail open: suppressed=%d count=%d", suppressed, strings.Count(string(got), unremembered))
	}

	long := telemetryHint(strings.Repeat("x", maxTelemetryHintBytes+1), "bashy")
	got, suppressed, _ = deduplicateTelemetryHints([]byte(long + long))
	if suppressed != 0 || string(got) != long+long {
		t.Fatal("overlong classified-looking lines were not retained")
	}
}

type maskRedactor struct{ values []string }

func (r maskRedactor) Redact(in []byte) []byte {
	out := bytes.Clone(in)
	for _, value := range r.values {
		out = bytes.ReplaceAll(out, []byte(value), []byte("[REDACTED]"))
	}
	return out
}

func TestReduceDeduplicatesAfterCanonicalizationAndRedaction(t *testing.T) {
	store := newTestStore(t)
	home := "/private/fixture/alice"
	secretA, secretB := "token-one-private", "token-two-private"
	first := telemetryHint(home+"/spans?token="+secretA, "bashy")
	second := telemetryHint(home+"/spans?token="+secretB, "bashy")
	in := []byte("start\n" + first + "middle\n" + second + second + "end\n")
	cfg := Config{BudgetBytes: 1024, HomeDir: home, Redactor: maskRedactor{[]string{secretA, secretB}}}

	res, err := Reduce(store, in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reduced || res.SuppressedHints != 2 {
		t.Fatalf("reduction accounting = %+v", res)
	}
	if len(res.Text) > cfg.BudgetBytes {
		t.Fatalf("view is %d bytes, budget is %d", len(res.Text), cfg.BudgetBytes)
	}
	if strings.Count(res.Text, "bashy: telemetry on") != 1 ||
		strings.Count(res.Text, "duplicate telemetry hints suppressed") != 1 ||
		!strings.Contains(res.Text, "full: bashy out "+res.Handle) {
		t.Fatalf("bad duplicate view: %q", res.Text)
	}
	for _, private := range []string{home, secretA, secretB} {
		if strings.Contains(res.Text, private) {
			t.Fatalf("private value reached inline output: %q", private)
		}
	}
	recovered, _, err := store.Get(res.Handle)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(string(in), home, "$HOME")
	want = strings.ReplaceAll(want, secretA, "[REDACTED]")
	want = strings.ReplaceAll(want, secretB, "[REDACTED]")
	if string(recovered) != want {
		t.Fatalf("recovery differs from complete pre-dedup artifact:\ngot  %q\nwant %q", recovered, want)
	}
	for _, private := range []string{home, secretA, secretB} {
		if bytes.Contains(recovered, []byte(private)) {
			t.Fatalf("private value reached recovery artifact: %q", private)
		}
	}

	again, err := Reduce(store, in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if again.Text != res.Text || again.Handle != res.Handle || again.Digest != res.Digest {
		t.Fatalf("recovery/view is not deterministic:\nfirst=%+v\nagain=%+v", res, again)
	}
}

func TestReduceDuplicateAnnotationMustFitTotalBudget(t *testing.T) {
	store := newTestStore(t)
	hint := telemetryHint("http://collector:4317", "bashy")
	res, err := Reduce(store, []byte(hint+hint), Config{BudgetBytes: 1})
	if err == nil || !strings.Contains(err.Error(), "recovery marker") {
		t.Fatalf("result=%+v err=%v, want pre-emit annotation error", res, err)
	}
	if res.Text != "" {
		t.Fatalf("annotation failure emitted %q", res.Text)
	}
	entries, readErr := bytesInStore(store)
	if readErr != nil || entries != 1 {
		t.Fatalf("complete artifact was not preserved before failure: entries=%d err=%v", entries, readErr)
	}
}

func TestTelemetryHintsOnlyDoesNotEnableGeneralElision(t *testing.T) {
	store := newTestStore(t)
	ordinary := []byte(strings.Repeat("ordinary output\n", 100))
	res, err := Reduce(store, ordinary, Config{BudgetBytes: 80, TelemetryHintsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reduced || !bytes.Equal([]byte(res.Text), ordinary) {
		t.Fatalf("telemetry-only mode changed ordinary output: %+v", res)
	}

	hint := telemetryHint("http://collector:4317", "bashy")
	res, err = Reduce(store, []byte(hint+hint+hint), Config{BudgetBytes: 512, TelemetryHintsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reduced || res.SuppressedHints != 2 || strings.Count(res.Text, hint) != 1 {
		t.Fatalf("telemetry-only mode did not suppress exact hints: %+v text=%q", res, res.Text)
	}
}

func TestReduceDuplicateAndHeadElisionShareTotalBudget(t *testing.T) {
	store := newTestStore(t)
	hint := telemetryHint("http://collector:4317", "bashy")
	in := []byte(hint + strings.Repeat("ordinary output that is not deduplicated\n", 100) + hint + hint)
	const budget = 240
	res, err := Reduce(store, in, Config{BudgetBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > budget {
		t.Fatalf("combined view is %d bytes, budget is %d", len(res.Text), budget)
	}
	if res.SuppressedHints != 2 || strings.Count(res.Text, "duplicate telemetry hints suppressed") != 1 {
		t.Fatalf("combined marker lost duplicate accounting: %+v text=%q", res, res.Text)
	}
	got, _, err := store.Get(res.Handle)
	if err != nil || !bytes.Equal(got, in) {
		t.Fatalf("combined recovery differs: err=%v", err)
	}
}

func bytesInStore(store *Store) (int, error) {
	entries, err := os.ReadDir(store.Root())
	return len(entries), err
}
