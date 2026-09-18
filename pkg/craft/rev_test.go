package craft

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRevisionAbsentStoreIsKnownEmpty(t *testing.T) {
	// A host that has learned nothing has a real, reportable state. It must not
	// be confused with a store we could not read — that distinction is the
	// whole reason Known exists.
	r := Revision(t.TempDir())
	if !r.Known {
		t.Fatal("an absent store is a KNOWN empty, not an unknown")
	}
	if !r.IsEmpty() {
		t.Fatalf("want empty, got %+v", r)
	}
	if r.String() == "" {
		t.Fatal("a known-empty rev must still render; only unknown renders empty")
	}
}

func TestRevisionUnsetDirIsUnknown(t *testing.T) {
	r := Revision("")
	if r.Known {
		t.Fatal("no store dir means we did not look, which is unknown")
	}
	if r.String() != "" {
		t.Fatalf("an unknown rev must render empty so GraphVersion stays absent, got %q", r.String())
	}
	if got := orUnknown(r.String()); got != "unknown" {
		t.Fatalf("provenance line should say unknown, got %q", got)
	}
}

func TestRevisionMovesWhenAStoreGrows(t *testing.T) {
	dir := t.TempDir()
	before := Revision(dir)

	appendLine(t, filepath.Join(dir, "folds.jsonl"), `{"coordinate":"c","note":"n"}`)
	afterFold := Revision(dir)
	if afterFold.String() == before.String() {
		t.Fatal("appending a fold must move the revision")
	}
	if afterFold.Folds == 0 {
		t.Fatalf("fold bytes not counted: %+v", afterFold)
	}

	appendLine(t, filepath.Join(dir, "facts.jsonl"), `{"entity":{"kind":"host","name":"h"},"key":"port"}`)
	afterFact := Revision(dir)
	if afterFact.String() == afterFold.String() {
		t.Fatal("appending a fact must move the revision")
	}

	// The empty-ledger case is exactly what byte size alone would miss, which
	// is why the ledger COUNT is carried.
	if err := os.MkdirAll(filepath.Join(dir, "attest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attest", "fresh.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	afterLedger := Revision(dir)
	if afterLedger.String() == afterFact.String() {
		t.Fatal("a new but empty attest ledger must still move the revision")
	}
	if afterLedger.Ledgers != 1 {
		t.Fatalf("ledger count = %d, want 1", afterLedger.Ledgers)
	}
}

func TestRevisionIsStableWithoutWrites(t *testing.T) {
	// The rev is a read, and a read that changes what it reports between two
	// identical calls cannot stamp anything.
	dir := t.TempDir()
	appendLine(t, filepath.Join(dir, "folds.jsonl"), `{"coordinate":"c","note":"n"}`)
	first := Revision(dir)
	for i := range 5 {
		if got := Revision(dir); got.String() != first.String() {
			t.Fatalf("call %d: %q != %q — revision is not stable", i, got.String(), first.String())
		}
	}
}

func TestRevisionCountsOnlyLedgerFiles(t *testing.T) {
	dir := t.TempDir()
	attest := filepath.Join(dir, "attest")
	if err := os.MkdirAll(filepath.Join(attest, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendLine(t, filepath.Join(attest, "one.jsonl"), `{"at":"now"}`)
	if err := os.WriteFile(filepath.Join(attest, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Revision(dir)
	if r.Ledgers != 1 {
		t.Fatalf("ledgers = %d, want 1 (a directory and a non-jsonl file are not ledgers)", r.Ledgers)
	}
}

// THE POINT OF THE WHOLE CHANGE. Two compositions can render byte-identical
// output from materially different stores — a fold at another coordinate, a
// skill absorbed since, an attestation accumulated. Before the rev had a
// producer the stamp hashed `graph=` empty and could not tell them apart, so a
// verdict attributed back to a stamp named a store state nobody recorded.
func TestCompose_StampSeparatesIdenticalBodiesFromDifferentStores(t *testing.T) {
	im := impl(t, "go-repo-health", goBuildTest, "verify a Go repo is healthy",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})

	dir := t.TempDir()
	early, err := Compose(im, ComposeOptions{Band: -1, GraphVersion: Revision(dir).String()})
	if err != nil {
		t.Fatal(err)
	}

	// A fold lands at a coordinate this composition does not read, so nothing
	// it applied has changed.
	appendLine(t, filepath.Join(dir, "folds.jsonl"), `{"coordinate":"elsewhere","note":"unrelated"}`)

	later, err := Compose(im, ComposeOptions{Band: -1, GraphVersion: Revision(dir).String()})
	if err != nil {
		t.Fatal(err)
	}

	if later.Body != early.Body {
		t.Fatal("precondition: the bodies must be identical for this test to mean anything")
	}
	if later.Stamp == early.Stamp {
		t.Fatal("same bytes from a moved store must stamp differently — the stamp addresses the READ, not only the output")
	}
	if early.GraphVersion == "" || later.GraphVersion == "" {
		t.Fatal("GraphVersion has a producer now; an empty one means it was not threaded")
	}
}

// Reproducibility still holds the other way: the same store, twice, is the same
// stamp. A rev that moved on its own would make every composition unique and
// the stamp worthless.
func TestCompose_StampStableAcrossReadsOfOneStore(t *testing.T) {
	im := impl(t, "go-repo-health", goBuildTest, "verify a Go repo is healthy",
		map[string]string{"check-build": "go build ./...", "check-tests": "go test ./..."})

	dir := t.TempDir()
	appendLine(t, filepath.Join(dir, "folds.jsonl"), `{"coordinate":"c","note":"n"}`)

	first, err := Compose(im, ComposeOptions{Band: -1, GraphVersion: Revision(dir).String()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compose(im, ComposeOptions{Band: -1, GraphVersion: Revision(dir).String()})
	if err != nil {
		t.Fatal(err)
	}
	if first.Stamp != second.Stamp {
		t.Fatalf("stamp is not stable: %s != %s", first.Stamp, second.Stamp)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}
