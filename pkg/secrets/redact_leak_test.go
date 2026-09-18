package secrets

import (
	"bytes"
	"strings"
	"testing"
)

const syntheticLeakFixture = "synthetic-redactor-fixture-2026"

func TestRedactorLeakVectorRatchet(t *testing.T) {
	vectors := []struct {
		name  string
		input string
	}{
		{name: `echo "$SECRET"`, input: syntheticLeakFixture + "\n"},
		{name: `${SECRET:-fallback}`, input: syntheticLeakFixture},
		{name: `${SECRET:+x}${SECRET:-y}`, input: "x" + syntheticLeakFixture},
		{name: `printf '%s' "$SECRET"`, input: syntheticLeakFixture},
		{name: "JSON output", input: `{"secret":"` + syntheticLeakFixture + `"}`},
		{name: "start of chunk", input: syntheticLeakFixture + " suffix"},
		{name: "end of chunk", input: "prefix " + syntheticLeakFixture},
		{name: "twice in one chunk", input: syntheticLeakFixture + ":" + syntheticLeakFixture},
	}

	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			captured := redactThroughWriter(t, "LEAK_FIXTURE", []string{vector.input})
			assertNoFixtureLeak(t, vector.name, -1, captured, syntheticLeakFixture)
			expected := strings.ReplaceAll(
				vector.input,
				syntheticLeakFixture,
				"[redacted:LEAK_FIXTURE]",
			)
			if captured != expected {
				t.Fatalf("leak vector %s produced unexpected redacted output", vector.name)
			}
		})
	}
}

func TestRedactorLeakRatchetEverySplitOffset(t *testing.T) {
	for offset := 0; offset <= len(syntheticLeakFixture); offset++ {
		chunks := []string{
			"prefix " + syntheticLeakFixture[:offset],
			syntheticLeakFixture[offset:] + " suffix",
		}
		captured := redactThroughWriter(t, "SPLIT_FIXTURE", chunks)
		assertNoFixtureLeak(t, "split across Write calls", offset, captured, syntheticLeakFixture)
		if captured != "prefix [redacted:SPLIT_FIXTURE] suffix" {
			t.Fatalf("split vector produced unexpected output at offset %d", offset)
		}
	}
}

func TestRedactorLeakRatchetMultilineValue(t *testing.T) {
	const multilineFixture = "synthetic-first-line\nsynthetic-second-line"

	// GHSA-4mgv-m5cm-f9h7 is the cautionary case: masking only the first
	// line of a multi-line secret still leaks credential material.
	captured := redactThroughWriter(t, "MULTILINE_FIXTURE", []string{
		"before " + multilineFixture[:12],
		multilineFixture[12:] + " after",
	})
	assertNoFixtureLeak(t, "multi-line value", 12, captured, multilineFixture)
	if strings.Contains(captured, "synthetic-first-line") ||
		strings.Contains(captured, "synthetic-second-line") {
		t.Fatal("multi-line value left a registered line in captured output")
	}
	if captured != "before [redacted:MULTILINE_FIXTURE] after" {
		t.Fatal("multi-line vector produced unexpected redacted output")
	}
}

func redactThroughWriter(t *testing.T, name string, chunks []string) string {
	t.Helper()

	r := NewRedactor()
	value := syntheticLeakFixture
	if name == "MULTILINE_FIXTURE" {
		value = "synthetic-first-line\nsynthetic-second-line"
	}
	if err := r.Register(name, value); err != nil {
		t.Fatalf("Register failed for vector %s: %v", name, err)
	}

	var captured bytes.Buffer
	w := r.Writer(&captured)
	for offset, chunk := range chunks {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write failed for vector %s at offset %d: %v", name, offset, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed for vector %s: %v", name, err)
	}
	return captured.String()
}

func assertNoFixtureLeak(t *testing.T, vector string, offset int, captured, value string) {
	t.Helper()
	if strings.Contains(captured, value) {
		t.Fatalf("leak vector %s exposed its fixture at split offset %d", vector, offset)
	}
}
