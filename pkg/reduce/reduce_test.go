// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

func TestReduceUnderBudgetKeepsEverything(t *testing.T) {
	store := newTestStore(t)
	in := []byte("a short line\nanother\n")
	res, err := Reduce(store, in, Config{BudgetBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reduced {
		t.Fatalf("small input should not be reduced")
	}
	if res.Marker != "" {
		t.Fatalf("no elision means no marker, got %q", res.Marker)
	}
	if res.Text != string(in) {
		t.Fatalf("text changed: %q", res.Text)
	}
	// Nothing should have been spilled.
	if store.Root() == "" {
		t.Fatalf("store lost its root")
	}
}

func TestReduceOverBudgetSpillsAndMarks(t *testing.T) {
	store := newTestStore(t)
	in := []byte(strings.Repeat("hello world\n", 5000)) // 60000 bytes, 5000 lines
	budget := 1024
	res, err := Reduce(store, in, Config{BudgetBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reduced {
		t.Fatalf("large input must be reduced")
	}
	if len(res.Text) > budget {
		t.Fatalf("reduced view %d bytes exceeds budget %d", len(res.Text), budget)
	}
	if !strings.Contains(res.Text, res.Marker) {
		t.Fatalf("reduced text must contain the marker")
	}
	if res.OmittedBytes != res.FullBytes-res.KeptBytes {
		t.Fatalf("omitted accounting: omitted=%d full=%d kept=%d", res.OmittedBytes, res.FullBytes, res.KeptBytes)
	}
	if res.OmittedLines <= 0 {
		t.Fatalf("expected elided lines, got %d", res.OmittedLines)
	}

	// Marker carries the runnable recovery command, the digest, and the
	// shell-path prevention hint.
	for _, want := range []string{
		"sha256:",
		"full: bashy out " + res.Handle,
		"keep: BASHY_OUTPUT_REDUCE=off",
		"lines /",
		"elided",
	} {
		if !strings.Contains(res.Marker, want) {
			t.Fatalf("marker %q missing %q", res.Marker, want)
		}
	}

	// The handle recovers the exact original bytes.
	got, digest, err := store.Get(res.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("recovered bytes differ from original")
	}
	if digest != res.Digest {
		t.Fatalf("digest mismatch: %s vs %s", digest, res.Digest)
	}
}

func TestReduceDeterministic(t *testing.T) {
	store := newTestStore(t)
	in := []byte(strings.Repeat("deterministic line\n", 4000))
	a, err := Reduce(store, in, Config{BudgetBytes: 800})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Reduce(store, in, Config{BudgetBytes: 800})
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != b.Text {
		t.Fatalf("non-deterministic reduction:\nA=%q\nB=%q", a.Text, b.Text)
	}
	if a.Marker != b.Marker {
		t.Fatalf("non-deterministic marker:\nA=%q\nB=%q", a.Marker, b.Marker)
	}
}

func TestReduceBinaryDetectedNotRepaired(t *testing.T) {
	store := newTestStore(t)
	// Invalid UTF-8: a lone continuation byte and a truncated multibyte lead.
	in := append([]byte("prefix"), 0xff, 0xfe, 0xc3)
	res, err := Reduce(store, in, Config{BudgetBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Binary {
		t.Fatalf("invalid UTF-8 must be flagged binary")
	}
	if !res.Reduced {
		t.Fatalf("binary output is always represented by a handle, never inlined")
	}
	if res.Text != res.Marker+"\n" {
		t.Fatalf("binary reduced view must be marker only, got %q", res.Text)
	}
	if !strings.Contains(res.Marker, "binary output elided") {
		t.Fatalf("binary marker text: %q", res.Marker)
	}
	// The raw bytes must not appear inline (not repaired, not inlined).
	if bytes.Contains([]byte(res.Text), []byte{0xff}) {
		t.Fatalf("raw binary leaked into the reduced view")
	}
	got, _, err := store.Get(res.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("binary recovery must be byte-exact")
	}
}

func TestReduceTinyBudgetsKeepMarkerBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		bin  bool
	}{
		{"binary", []byte{0xff, 0xfe, 0xfd}, true},
		{"text", []byte(strings.Repeat("line one\n", 2000)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			digest, err := store.Put(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			probe := Result{Binary: tc.bin, Digest: digest, Handle: store.shortestHandle(strings.TrimPrefix(digest, digestPrefix)), OmittedBytes: len(tc.in), OmittedLines: countLines(tc.in)}
			markerLen := len(buildMarker(probe, DefaultRecoverVerb, DefaultKeepHint))
			for _, budget := range []int{1, markerLen - 1, markerLen, markerLen + 1} {
				res, err := Reduce(store, tc.in, Config{BudgetBytes: budget})
				if budget < markerLen {
					if err == nil || !strings.Contains(err.Error(), "recovery marker") {
						t.Fatalf("budget %d: expected marker-fit error, got result=%+v err=%v", budget, res, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("budget %d: %v", budget, err)
				}
				if len(res.Text) > budget {
					t.Fatalf("budget %d: text length %d", budget, len(res.Text))
				}
				if strings.HasPrefix(res.Text, "\n") {
					t.Fatalf("budget %d: stray leading newline in %q", budget, res.Text)
				}
				if budget == markerLen && res.Text != res.Marker {
					t.Fatalf("marker-only budget emitted framing: %q", res.Text)
				}
				if budget == markerLen+1 && res.Text != res.Marker+"\n" {
					t.Fatalf("marker-plus-newline budget emitted %q", res.Text)
				}
			}
		})
	}
}

func TestReduceSpillFailureBeforeEmit(t *testing.T) {
	blocked := t.TempDir() + "/blocked"
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root := blocked + "/child"
	res, err := Reduce(NewStore(root), []byte(strings.Repeat("x", 100)), Config{BudgetBytes: 1})
	if err == nil || !strings.Contains(err.Error(), "spill failed before emit") {
		t.Fatalf("expected spill failure before emit, result=%+v err=%v", res, err)
	}
	if res.Text != "" {
		t.Fatalf("spill failure emitted text %q", res.Text)
	}
}

type upperRedactor struct{}

// Redact stands in for a secret gate: it demonstrably transforms the bytes so a
// test can prove redaction happened BEFORE the spill.
func (upperRedactor) Redact(b []byte) []byte {
	return bytes.ToUpper(b)
}

func TestReduceRedactionPrecedesSpill(t *testing.T) {
	store := newTestStore(t)
	in := []byte(strings.Repeat("secret token line\n", 3000))
	res, err := Reduce(store, in, Config{BudgetBytes: 512, Redactor: upperRedactor{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reduced {
		t.Fatalf("expected reduction")
	}
	// The spilled artifact is the redacted content, not the raw input.
	got, _, err := store.Get(res.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("secret token")) {
		t.Fatalf("unredacted bytes reached the spill")
	}
	if !bytes.Contains(got, []byte("SECRET TOKEN")) {
		t.Fatalf("spill should hold the redacted bytes")
	}
	// The inline head is redacted too.
	if strings.Contains(res.Text, "secret token") {
		t.Fatalf("unredacted bytes reached the reduced view")
	}
}

func TestReduceRequiresStoreWhenEliding(t *testing.T) {
	in := []byte(strings.Repeat("x\n", 5000))
	if _, err := Reduce(nil, in, Config{BudgetBytes: 64}); err == nil {
		t.Fatalf("eliding without a store must fail loudly")
	}
	// A nil store is fine when nothing needs spilling.
	res, err := Reduce(nil, []byte("tiny"), Config{BudgetBytes: 64})
	if err != nil {
		t.Fatalf("small input with nil store should succeed: %v", err)
	}
	if res.Reduced {
		t.Fatalf("small input should not reduce")
	}
}

func TestCustomVerbAndHint(t *testing.T) {
	store := newTestStore(t)
	in := []byte(strings.Repeat("row\n", 4000))
	res, err := Reduce(store, in, Config{BudgetBytes: 512, RecoverVerb: "outpost out", KeepHint: "NO_ELIDE=1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Marker, "full: outpost out "+res.Handle) {
		t.Fatalf("custom verb missing: %q", res.Marker)
	}
	if !strings.Contains(res.Marker, "keep: NO_ELIDE=1") {
		t.Fatalf("custom hint missing: %q", res.Marker)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int]string{
		0:           "0 B",
		512:         "512 B",
		1024:        "1 KiB",
		400384:      "391 KiB",
		1024 * 1024: "1 MiB",
		3 * 1 << 30: "3 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d)=%q want %q", n, got, want)
		}
	}
}

func TestCommaInt(t *testing.T) {
	cases := map[int]string{0: "0", 42: "42", 4812: "4,812", 1000000: "1,000,000", -1234: "-1,234"}
	for n, want := range cases {
		if got := commaInt(n); got != want {
			t.Errorf("commaInt(%d)=%q want %q", n, got, want)
		}
	}
}

func TestCountLines(t *testing.T) {
	cases := map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\n": 1, "\n\n": 2}
	for in, want := range cases {
		if got := countLines([]byte(in)); got != want {
			t.Errorf("countLines(%q)=%d want %d", in, got, want)
		}
	}
}
