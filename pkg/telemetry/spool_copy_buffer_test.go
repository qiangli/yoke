package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The long line finishes inside a scanner chunk that still contains later
// records. Reusing that chunk for copyRange corrupts its unprocessed suffix.
func TestCompactionLongLineDoesNotOverwriteScannerSuffix(t *testing.T) {
	var input bytes.Buffer
	prefix := `{"pad":"` + strings.Repeat("p", 500) + `"}` + "\n"
	for i := 0; i < 6000; i++ {
		input.WriteString(prefix)
	}
	large := []byte(`{"large":"` + strings.Repeat("x", healMaxLine+5000) + `"}` + "\n")
	input.Write(large)
	var suffix bytes.Buffer
	for i := 0; i < 100; i++ {
		b, _ := json.Marshal(map[string]any{"suffix": i})
		suffix.Write(b)
		suffix.WriteByte('\n')
	}
	input.Write(suffix.Bytes())
	path := filepath.Join(t.TempDir(), "spans.jsonl")
	if err := os.WriteFile(path, input.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if dropped, err := compactSpoolInPlace(f, int64(input.Len())); err != nil || dropped != 0 {
		t.Fatalf("compaction: dropped=%d err=%v", dropped, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, large) || !bytes.HasSuffix(got, suffix.Bytes()) {
		t.Fatal("long-line copy corrupted original line or following records")
	}
	for _, line := range bytes.Split(bytes.TrimSuffix(got, []byte("\n")), []byte("\n")) {
		if !json.Valid(line) {
			t.Fatal("invalid JSON after long-line copy")
		}
	}
}
