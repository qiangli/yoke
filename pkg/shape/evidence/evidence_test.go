package evidence

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func pass(candidate, base string) Record {
	return Record{
		Binding: Binding{
			Candidate: candidate, Base: base, Verifier: "shape-verify/1+test",
			Contract: "sha256:contract", Dependency: "sha256:deps", Principal: "os:user",
		},
		Verdict: VerdictPass,
		Checks:  []CheckResult{{Name: "checks/smoke.bsh", Status: StatusPass}},
	}
}

func TestSealedRecordRoundTrips(t *testing.T) {
	home := t.TempDir()
	rec := pass("c1", "b1")
	p, err := Write(home, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, filepath.Join(home, "src", ".evidence")) {
		t.Fatalf("record at %s, want it under src/.evidence", p)
	}
	got, err := Require(home, rec.Binding)
	if err != nil {
		t.Fatalf("Require: %v", err)
	}
	if got.Verdict != VerdictPass || got.Binding != rec.Binding {
		t.Fatalf("record = %+v, want the one written", got)
	}
}

// The host key is the whole basis of the seal. It must not be readable by
// anyone but its owner, and it must be stable across calls.
func TestHostKeyIsOwnerOnlyAndStable(t *testing.T) {
	home := t.TempDir()
	first, err := LocalHostKey(home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LocalHostKey(home)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("the host key changed between calls")
	}
	if len(first) < 32 {
		t.Fatalf("host key is %d bytes, want at least 32", len(first))
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(Dir(home), hostKeyName))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("host key mode = %v, want 0600", st.Mode().Perm())
		}
	}
}

// Every field is sealed, so editing any of them is forgery rather than a
// weaker-but-usable record.
func TestEditingAnyFieldIsForgery(t *testing.T) {
	for _, edit := range []struct {
		name string
		do   func(map[string]any)
	}{
		{"verdict", func(m map[string]any) { m["verdict"] = VerdictPass }},
		{"candidate", func(m map[string]any) {
			m["binding"].(map[string]any)["candidate"] = "c9"
		}},
		{"principal", func(m map[string]any) {
			m["binding"].(map[string]any)["principal"] = "os:someone"
		}},
		{"checks", func(m map[string]any) { m["checks"] = []any{} }},
		{"seal removed", func(m map[string]any) { m["seal"] = "" }},
	} {
		t.Run(edit.name, func(t *testing.T) {
			home := t.TempDir()
			rec := pass("c1", "b1")
			rec.Verdict = VerdictFail
			rec.Checks = []CheckResult{{Name: "checks/smoke.bsh", Status: StatusFail}}
			if _, err := Write(home, rec); err != nil {
				t.Fatal(err)
			}
			p := Path(home, "c1")
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			edit.do(m)
			out, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, out, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Require(home, rec.Binding); !errors.Is(err, ErrForged) {
				t.Fatalf("err = %v, want ErrForged", err)
			}
		})
	}
}

// A record copied onto another candidate's filename is refused: the
// filename is a claim, the seal is the fact.
func TestRecordMovedOntoAnotherCandidate(t *testing.T) {
	home := t.TempDir()
	rec := pass("c1", "b1")
	if _, err := Write(home, rec); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(Path(home, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(home, "c2"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(home, "c2"); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
}

// Each part of the binding is a prerequisite: change it and the record no
// longer describes what is being asked about.
func TestEveryBindingFieldGoesStale(t *testing.T) {
	home := t.TempDir()
	rec := pass("c1", "b1")
	if _, err := Write(home, rec); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		want Binding
	}{
		{"base", Binding{Candidate: "c1", Base: "b2"}},
		{"verifier", Binding{Candidate: "c1", Verifier: "shape-verify/2+other"}},
		{"contract", Binding{Candidate: "c1", Contract: "sha256:other"}},
		{"dependency", Binding{Candidate: "c1", Dependency: "sha256:other"}},
		{"principal", Binding{Candidate: "c1", Principal: "os:other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Require(home, tc.want); !errors.Is(err, ErrStale) {
				t.Fatalf("err = %v, want ErrStale", err)
			}
		})
	}
	// An empty field is "not checked here", not "matches anything it likes":
	// the fields the caller did supply still have to match.
	if _, err := Require(home, Binding{Candidate: "c1"}); err != nil {
		t.Fatalf("partial binding: %v", err)
	}
}

func TestMissingAndFailedVerdicts(t *testing.T) {
	home := t.TempDir()
	if _, err := Require(home, Binding{Candidate: "nope"}); !errors.Is(err, ErrMissing) {
		t.Fatalf("err = %v, want ErrMissing", err)
	}
	rec := pass("c1", "b1")
	rec.Verdict = VerdictFail
	rec.Checks = []CheckResult{{Name: "checks/smoke.bsh", Status: StatusSkip}}
	rec.Uncalled = []string{"report"}
	if _, err := Write(home, rec); err != nil {
		t.Fatal(err)
	}
	_, err := Require(home, rec.Binding)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("err = %v, want ErrFailed", err)
	}
	if !strings.Contains(err.Error(), "report") {
		t.Fatalf("err = %v, want it to name the uncalled function", err)
	}
}

// A record minted under a different host key does not verify here. This is
// what stops a record being carried from one host to another.
func TestARecordFromAnotherHostDoesNotVerify(t *testing.T) {
	home := t.TempDir()
	rec := pass("c1", "b1")
	if _, err := Write(home, rec); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(Dir(home), hostKeyName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Require(home, rec.Binding); !errors.Is(err, ErrForged) {
		t.Fatalf("err = %v, want ErrForged under a new host key", err)
	}
}

// Only the verifier mints records. Nothing else in the module may call
// Write: a second writer is a second thing that can declare a candidate
// verified, which is the one property this package exists to hold.
func TestOnlyTheVerifierWritesRecords(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "external", "testdata", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if !strings.Contains(string(raw), "evidence.Write(") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if filepath.ToSlash(filepath.Dir(rel)) != "pkg/shape/verify" {
			offenders = append(offenders, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("evidence.Write called outside pkg/shape/verify: %s", strings.Join(offenders, ", "))
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
