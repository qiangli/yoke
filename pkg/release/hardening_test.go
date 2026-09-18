// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A known-but-unset field is the "ycode--.tar.gz" hazard the package promises
// to refuse. Only an UNKNOWN field used to error, so this went undetected.
func TestRenderRefusesUnsetKnownField(t *testing.T) {
	if _, err := Render("ycode-{{ .Os }}-{{ .Arch }}", Fields{ProjectName: "ycode"}); err == nil {
		t.Fatal("an unset Os/Arch must error, not render an empty segment")
	}
	// Env is a map: an absent key is the same class of silent gap.
	if _, err := Render("{{ .Env.NOPE }}", Fields{ProjectName: "ycode"}); err == nil {
		t.Fatal("an absent Env key must error")
	}
	got, err := Render("{{ .Env.FLAVOR }}", Fields{Env: map[string]string{"FLAVOR": "lean"}})
	if err != nil || got != "lean" {
		t.Fatalf("Env lookup = %q, %v", got, err)
	}
}

// The checksum template is rendered with no target bound, so a per-target
// template there silently produced a manifest named "ycode--".
func TestPlanRefusesTargetFieldsInChecksumTemplate(t *testing.T) {
	cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    goos: [linux]
    goarch: [amd64]
archives:
  - name_template: '{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}'
checksum:
  name_template: '{{ .ProjectName }}-{{ .Os }}-sums.txt'
`)
	if _, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.0.0"}); err == nil {
		t.Fatal("checksum.name_template referencing .Os must be refused")
	}
}

// The manifest is written after the archives are hashed, so a collision
// overwrites a shipped archive while the ledger still carries its digest.
func TestPlanRefusesChecksumCollidingWithArchive(t *testing.T) {
	cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    goos: [linux]
    goarch: [amd64]
archives:
  - formats: [binary]
    name_template: SHA256SUMS
checksum:
  name_template: SHA256SUMS
`)
	_, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("want collision refusal naming the checksum manifest, got %v", err)
	}
	if !strings.Contains(err.Error(), "checksum manifest") {
		t.Fatalf("error must say which side collided: %v", err)
	}
}

// Artifact names are joined onto dist/; a template resolving to a path would
// write outside the output directory.
func TestPlanRefusesNameEscapingDist(t *testing.T) {
	for _, tmpl := range []string{"../{{ .Os }}", "nested/{{ .Os }}", `back\{{ .Os }}`} {
		cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    goos: [linux]
    goarch: [amd64]
archives:
  - name_template: '`+tmpl+`'
`)
		_, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.0.0"})
		if err == nil || !strings.Contains(err.Error(), "plain file names") {
			t.Fatalf("name_template %q must be refused as a path, got %v", tmpl, err)
		}
	}
}

// A typo'd target used to be skipped, shrinking the matrix with nothing
// reporting that a platform had been dropped.
func TestMatrixRefusesMalformedTarget(t *testing.T) {
	cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    targets: [linux_amd64, windows]
archives:
  - name_template: '{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}'
`)
	_, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "windows") {
		t.Fatalf("a malformed target must fail closed and name itself, got %v", err)
	}
}

func TestPlanAcceptsWellFormedTargets(t *testing.T) {
	cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    targets: [linux_amd64, darwin_arm64]
archives:
  - name_template: '{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}'
`)
	plan, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Archives) != 2 {
		t.Fatalf("targets: produced %d archives, want 2", len(plan.Archives))
	}
}

// Two files with the same base name would be packed under one member name;
// the second copy shadows the first, so the archive holds content the config
// did not describe.
func TestPlanRefusesDuplicateArchiveMembers(t *testing.T) {
	cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    goos: [linux]
    goarch: [amd64]
archives:
  - name_template: '{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}'
    files: [docs/README.md, README.md]
`)
	_, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "README.md") {
		t.Fatalf("want duplicate-member refusal naming the file, got %v", err)
	}
}

func TestCheckMemberNameRefusesTraversal(t *testing.T) {
	for _, bad := range []string{"", "../etc/passwd", "/etc/passwd", "a/../../b", `win\path`} {
		if err := checkMemberName(bad); err == nil {
			t.Fatalf("member name %q must be refused", bad)
		}
	}
	for _, ok := range []string{"ycode", "ycode.exe", "ycode-1.0/ycode", "a..b"} {
		if err := checkMemberName(ok); err != nil {
			t.Fatalf("member name %q must be accepted: %v", ok, err)
		}
	}
}

// A config env: entry must not be able to redirect the matrix. Compiling
// linux bytes and archiving them as darwin is a green release that strands
// every darwin host.
func TestBuildEnvPinsMatrixOverConfigEnv(t *testing.T) {
	env := buildEnv(
		[]string{"PATH=/bin", "GOOS=freebsd"},
		Target{Goos: "darwin", Goarch: "arm64", Env: []string{"GOOS=linux", "GOARCH=386", "CGO_ENABLED=1"}},
	)
	if got := lastValue(env, "GOOS"); got != "darwin" {
		t.Fatalf("GOOS = %q, want darwin — config env must not override the matrix", got)
	}
	if got := lastValue(env, "GOARCH"); got != "arm64" {
		t.Fatalf("GOARCH = %q, want arm64", got)
	}
	// CGO_ENABLED is a default, not a pin: a build that needs cgo may ask.
	if got := lastValue(env, "CGO_ENABLED"); got != "1" {
		t.Fatalf("CGO_ENABLED = %q, want the config's 1", got)
	}
	if got := lastValue(env, "PATH"); got != "/bin" {
		t.Fatalf("inherited PATH lost: %q", got)
	}
}

func TestBuildEnvDefaultsCgoOff(t *testing.T) {
	env := buildEnv(nil, Target{Goos: "linux", Goarch: "amd64"})
	if got := lastValue(env, "CGO_ENABLED"); got != "0" {
		t.Fatalf("CGO_ENABLED = %q, want 0 by default", got)
	}
}

// lastValue mirrors exec's duplicate-key rule: the last entry wins.
func lastValue(env []string, key string) string {
	out := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			out = v
		}
	}
	return out
}

// The checksum manifest must be byte-stable regardless of the order artifacts
// were produced in.
func TestChecksumManifestIsOrderStable(t *testing.T) {
	dir := t.TempDir()
	arts := []Artifact{
		{Name: "b.tar.gz", Type: "archive", SHA256: strings.Repeat("2", 64)},
		{Name: "a.tar.gz", Type: "archive", SHA256: strings.Repeat("1", 64)},
		{Name: "SHA256SUMS", Type: "checksum", SHA256: strings.Repeat("9", 64)},
	}
	one := filepath.Join(dir, "one")
	two := filepath.Join(dir, "two")
	if err := writeChecksums(one, arts); err != nil {
		t.Fatal(err)
	}
	if err := writeChecksums(two, []Artifact{arts[1], arts[2], arts[0]}); err != nil {
		t.Fatal(err)
	}
	a, b := readFile(t, one), readFile(t, two)
	if string(a) != string(b) {
		t.Fatalf("manifest depends on artifact order:\n%s\n---\n%s", a, b)
	}
	want := strings.Repeat("1", 64) + "  a.tar.gz\n" + strings.Repeat("2", 64) + "  b.tar.gz\n"
	if string(a) != want {
		t.Fatalf("manifest = %q, want %q (checksum file itself must not be listed)", a, want)
	}
}

// The T0 exit criterion in full: the same inputs must produce the same
// artifact bytes, so "reproduces release.yml byte-for-byte" is checkable.
// TestArchiveIsDeterministic covers one archive; this covers a whole run,
// including the manifest and the digests recorded in the ledger.
func TestRunIsReproducible(t *testing.T) {
	run := func() *Ledger {
		t.Helper()
		dir := t.TempDir()
		led, err := Run(t.Context(), mustConfig(t, ycodeConfig), Options{
			Dir: dir, Builder: &fakeBuilder{}, Snapshot: true,
			Fields: Fields{Version: "0.1.0", ShortCommit: "abc1234"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return led
	}
	a, b := run(), run()
	if len(a.Artifacts) != len(b.Artifacts) {
		t.Fatalf("artifact count differs: %d vs %d", len(a.Artifacts), len(b.Artifacts))
	}
	for i := range a.Artifacts {
		x, y := a.Artifacts[i], b.Artifacts[i]
		if x.Name != y.Name {
			t.Fatalf("artifact %d name differs: %q vs %q", i, x.Name, y.Name)
		}
		if x.SHA256 != y.SHA256 {
			t.Fatalf("%s is not reproducible: %s vs %s", x.Name, x.SHA256, y.SHA256)
		}
		if x.Size != y.Size {
			t.Fatalf("%s size differs: %d vs %d", x.Name, x.Size, y.Size)
		}
	}
}

// A run must not leave artifacts outside dist/.
func TestRunWritesOnlyIntoDist(t *testing.T) {
	dir := t.TempDir()
	cfg := mustConfig(t, ycodeConfig)
	if _, err := Run(t.Context(), cfg, Options{
		Dir: dir, Builder: &fakeBuilder{}, Snapshot: true,
		Fields: Fields{Version: "0.1.0"},
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "dist" {
			t.Fatalf("run wrote %q outside dist/", e.Name())
		}
	}
}

// Ldflags carry the version stamp, so they must be rendered like any other
// template. Leaving them literal has two failure modes and only one is loud:
// `-X main.version={{ .Version }}` (spaced) makes the linker split on the
// spaces and reject `.Version` as a flag, while `-X main.version={{.Version}}`
// is accepted verbatim and the shipped binary REPORTS "{{.Version}}" as its
// version. The second is a green release that lies about what it is.
func TestPlanRendersLdflagsAgainstTheTargetsFields(t *testing.T) {
	cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    targets: [linux_amd64]
    ldflags:
      - -s -w -X main.version={{ .Version }} -X main.commit={{ .Commit }} -X main.os={{ .Os }}
archives:
  - name_template: '{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}'
`)
	plan, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.2.3", Commit: "deadbeef"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(plan.Targets[0].Ldflags, " ")
	for _, want := range []string{"main.version=1.2.3", "main.commit=deadbeef", "main.os=linux"} {
		if !strings.Contains(got, want) {
			t.Errorf("ldflags = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("an unrendered template reached the builder: %q", got)
	}
}

// Build tags are their own field rather than something smuggled through flags,
// because a project whose binary is only correct under a tag set would
// otherwise depend on the caller splitting a string the same way the go tool
// does. One -tags argument, comma-joined, is what the go tool accepts.
func TestPlanCarriesBuildTagsToTheTarget(t *testing.T) {
	cfg := mustConfig(t, `
project_name: ycode
builds:
  - id: ycode
    targets: [linux_amd64]
    tags: [sqlite, embed_spawn]
archives:
  - name_template: '{{ .ProjectName }}-{{ .Os }}-{{ .Arch }}'
`)
	plan, err := BuildPlan(cfg, Fields{ProjectName: "ycode", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.Targets[0].Tags, ","); got != "sqlite,embed_spawn" {
		t.Errorf("tags = %q, want sqlite,embed_spawn", got)
	}
}
