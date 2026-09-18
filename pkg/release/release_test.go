// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package release

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigRefusesUnimplementedStages(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"dockers", "dockers:\n  - image_templates: [x]\n", "dockers"},
		{"announce", "announce:\n  slack:\n    enabled: true\n", "announce"},
		{"blobs", "blobs:\n  - provider: s3\n", "blobs"},
		{"signs", "signs:\n  - cmd: cosign\n", "signs"},
		{"build hooks", "builds:\n  - id: a\n    hooks:\n      pre: echo hi\n", "hooks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.yaml))
			if !errors.Is(err, ErrUnsupportedStage) {
				t.Fatalf("want ErrUnsupportedStage, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error must name the stage %q: %v", tc.want, err)
			}
		})
	}
}

func TestParseConfigRejectsUnknownKey(t *testing.T) {
	// An unrecognised key is a stage we would silently skip. Fail closed.
	if _, err := ParseConfig([]byte("wobbles:\n  - nope\n")); err == nil {
		t.Fatal("unknown key must be an error")
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("project_name: demo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyDefaults("/tmp/demo"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Builds) != 1 || cfg.Builds[0].ID != "default" || cfg.Builds[0].Binary != "demo" {
		t.Fatalf("build defaults: %+v", cfg.Builds)
	}
	if got := len(cfg.Builds[0].Goos) * len(cfg.Builds[0].Goarch); got != 6 {
		t.Fatalf("default matrix = %d cells, want 6", got)
	}
	if cfg.Dist != "dist" || cfg.Checksum.Algorithm != "sha256" {
		t.Fatalf("defaults: dist=%q algo=%q", cfg.Dist, cfg.Checksum.Algorithm)
	}
}

func TestRenderFailsClosedOnUnknownField(t *testing.T) {
	if _, err := Render("{{ .Nope }}", Fields{Version: "v1"}); err == nil {
		t.Fatal("unknown template field must error, not render empty")
	}
	got, err := Render("{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}", Fields{ProjectName: "ycode", Os: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "ycode-linux-amd64" {
		t.Fatalf("got %q", got)
	}
}

// ycodeConfig expresses today's release.yml naming as a .goreleaser.yaml
// subset: ycode-<os>-<arch>.tar.gz holding a binary named `ycode`, plus
// SHA256SUMS. These names are consumed BY NAME by the fleet-upgrade contract
// (cloudbox/outpost), so they are a compatibility surface, not a preference.
const ycodeConfig = `
project_name: ycode
builds:
  - id: ycode
    main: ./cmd/ycode
    binary: ycode
    goos: [linux, darwin]
    goarch: [amd64, arm64]
    ignore:
      - goos: darwin
        goarch: amd64
archives:
  - id: ycode
    formats: [tar.gz]
    name_template: '{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}'
checksum:
  name_template: SHA256SUMS
`

func TestPlanReproducesYcodeArtifactNames(t *testing.T) {
	cfg := mustConfig(t, ycodeConfig)
	plan, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "0.1.0", Tag: "v0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range plan.Archives {
		names = append(names, a.Name)
	}
	want := []string{"ycode-linux-amd64.tar.gz", "ycode-linux-arm64.tar.gz", "ycode-darwin-arm64.tar.gz"}
	if len(names) != len(want) {
		t.Fatalf("archives = %v, want %v", names, want)
	}
	for _, w := range want {
		if !containsStr(names, w) {
			t.Fatalf("missing artifact %q in %v", w, names)
		}
	}
	if plan.ChecksumName != "SHA256SUMS" {
		t.Fatalf("checksum name = %q", plan.ChecksumName)
	}
	// darwin/amd64 is ignored in release.yml today; the plan must honour it.
	if containsStr(names, "ycode-darwin-amd64.tar.gz") {
		t.Fatal("ignore: rule was not applied")
	}
}

func TestPlanRefusesNameCollision(t *testing.T) {
	cfg := mustConfig(t, `
project_name: demo
builds:
  - id: demo
    goos: [linux]
    goarch: [amd64, arm64]
archives:
  - name_template: '{{ .ProjectName }}'
`)
	_, err := BuildPlan(cfg, Fields{ProjectName: "demo", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("want collision refusal, got %v", err)
	}
}

// fakeBuilder writes deterministic bytes instead of compiling, so the
// pipeline test needs no Go toolchain and no network.
type fakeBuilder struct{ calls int }

func (f *fakeBuilder) Build(_ context.Context, t Target) error {
	f.calls++
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(t.Path, []byte("BIN "+t.Goos+"/"+t.Goarch+"\n"), 0o755)
}

// deadBuilder reports success and produces nothing — the shape the evidence
// invariant exists to catch.
type deadBuilder struct{}

func (deadBuilder) Build(context.Context, Target) error { return nil }

func TestRunProducesYcodeAssets(t *testing.T) {
	dir := t.TempDir()
	cfg := mustConfig(t, ycodeConfig)
	fb := &fakeBuilder{}
	led, err := Run(context.Background(), cfg, Options{
		Dir:      dir,
		Builder:  fb,
		Snapshot: true,
		Fields:   Fields{Version: "0.1.0-SNAPSHOT", Tag: "v0.1.0-dev", ShortCommit: "abc1234"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fb.calls != 3 {
		t.Fatalf("builder called %d times, want 3", fb.calls)
	}
	if led.Schema != LedgerSchema || !led.Snapshot {
		t.Fatalf("ledger header: %+v", led)
	}
	if len(led.Artifacts) != 4 {
		t.Fatalf("artifacts = %d, want 3 archives + 1 checksum", len(led.Artifacts))
	}
	sums, err := os.ReadFile(filepath.Join(dir, "dist", "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
	if len(lines) != 3 {
		t.Fatalf("SHA256SUMS has %d lines: %q", len(lines), sums)
	}
	for _, l := range lines {
		// sha256sum(1) format: 64 hex chars, two spaces, name.
		if len(l) < 67 || l[64:66] != "  " {
			t.Fatalf("not sha256sum format: %q", l)
		}
	}
	// The manifest must be sorted, so two runs of the same inputs are
	// byte-identical.
	if !sortedNames(lines) {
		t.Fatalf("SHA256SUMS not sorted: %q", sums)
	}
}

func TestRunFailsClosedWhenBuilderProducesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := mustConfig(t, ycodeConfig)
	_, err := Run(context.Background(), cfg, Options{
		Dir: dir, Builder: deadBuilder{}, Snapshot: true,
		Fields: Fields{Version: "0.1.0"},
	})
	if err == nil || !strings.Contains(err.Error(), "produced no binary") {
		t.Fatalf("a builder that exits 0 with no output must fail the run, got %v", err)
	}
}

func TestRunRequiresVersion(t *testing.T) {
	cfg := mustConfig(t, ycodeConfig)
	if _, err := Run(context.Background(), cfg, Options{Dir: t.TempDir(), Builder: &fakeBuilder{}}); err == nil {
		t.Fatal("empty version must be refused")
	}
}

// TestArchiveIsDeterministic is the T0 exit criterion in miniature: the same
// input bytes must produce the same archive bytes, whatever the file's mtime.
func TestArchiveIsDeterministic(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "ycode")
			if err := os.WriteFile(src, []byte("hello"), 0o755); err != nil {
				t.Fatal(err)
			}
			a := filepath.Join(dir, "a."+format)
			if err := WriteArchive(a, format, []Member{{Name: "ycode", Source: src, Mode: 0o755}}); err != nil {
				t.Fatal(err)
			}
			// Touch the source: a real mtime would leak into the archive.
			if err := os.Chtimes(src, epoch.AddDate(5, 0, 0), epoch.AddDate(5, 0, 0)); err != nil {
				t.Fatal(err)
			}
			b := filepath.Join(dir, "b."+format)
			if err := WriteArchive(b, format, []Member{{Name: "ycode", Source: src, Mode: 0o755}}); err != nil {
				t.Fatal(err)
			}
			ab, bb := readFile(t, a), readFile(t, b)
			if !bytes.Equal(ab, bb) {
				t.Fatalf("%s archive is not deterministic (%d vs %d bytes)", format, len(ab), len(bb))
			}
		})
	}
}

func TestArchiveRefusesUnknownFormat(t *testing.T) {
	err := WriteArchive(filepath.Join(t.TempDir(), "x.rar"), "rar", nil)
	if !errors.Is(err, ErrUnsupportedStage) {
		t.Fatalf("want ErrUnsupportedStage, got %v", err)
	}
}

func TestMemberOrderDoesNotChangeBytes(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one")
	two := filepath.Join(dir, "two")
	if err := os.WriteFile(one, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	m1 := []Member{{Name: "one", Source: one, Mode: 0o644}, {Name: "two", Source: two, Mode: 0o644}}
	m2 := []Member{m1[1], m1[0]}
	a, b := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "b.tar.gz")
	if err := WriteArchive(a, "tar.gz", m1); err != nil {
		t.Fatal(err)
	}
	if err := WriteArchive(b, "tar.gz", m2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, a), readFile(t, b)) {
		t.Fatal("member order changed the archive bytes")
	}
}

func mustConfig(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyDefaults(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedNames(lines []string) bool {
	for i := 1; i < len(lines); i++ {
		if lines[i-1][66:] > lines[i][66:] {
			return false
		}
	}
	return true
}
