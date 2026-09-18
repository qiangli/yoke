// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// LedgerSchema is the schema tag on the emitted artifact ledger.
const LedgerSchema = "bashy-release-v1"

// Artifact is one produced file. This is the typed noun Tier 1's smoke,
// publish, and fleet-envelope stages consume.
type Artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Type   string `json:"type"` // archive | checksum
	Goos   string `json:"goos,omitempty"`
	Goarch string `json:"goarch,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Ledger is the machine-readable result of a run.
type Ledger struct {
	Schema      string     `json:"schema_version"`
	ProjectName string     `json:"project_name"`
	Version     string     `json:"version"`
	Tag         string     `json:"tag,omitempty"`
	Snapshot    bool       `json:"snapshot"`
	Dist        string     `json:"dist"`
	Artifacts   []Artifact `json:"artifacts"`
}

// Builder compiles one target. The interface is the seam that keeps this
// package testable without a Go toolchain in the test environment.
type Builder interface {
	Build(ctx context.Context, t Target) error
}

// GoBuilder shells out to the Go toolchain. It is the only exec in this
// package: compiling Go is not a coreutils tool being reimplemented, it is the
// toolchain doing its own job (the same call `bashy go` provisions).
type GoBuilder struct {
	// GoBin is the go binary; defaults to "go".
	GoBin string
	// Stderr receives the compiler's diagnostics.
	Stderr io.Writer
}

// Build compiles t.Main into t.Path for t's platform.
func (g GoBuilder) Build(ctx context.Context, t Target) error {
	bin := g.GoBin
	if bin == "" {
		bin = "go"
	}
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(t.Path)
	if err != nil {
		return err
	}
	args := []string{"build", "-o", abs}
	args = append(args, t.Flags...)
	if tags := strings.TrimSpace(strings.Join(t.Tags, ",")); tags != "" {
		args = append(args, "-tags", tags)
	}
	if ld := strings.TrimSpace(strings.Join(t.Ldflags, " ")); ld != "" {
		args = append(args, "-ldflags", ld)
	}
	args = append(args, t.Main)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = t.Dir
	cmd.Env = buildEnv(os.Environ(), t)
	cmd.Stderr = g.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s/%s: %w", t.Goos, t.Goarch, err)
	}
	return nil
}

// buildEnv assembles the child environment for one target.
//
// Order is load-bearing: on a duplicate key the LAST entry wins. The config's
// env comes after the CGO_ENABLED default, so a build that genuinely needs cgo
// can ask for it — but BEFORE the matrix pins, because a config that sets GOOS
// or GOARCH would otherwise compile one platform's binary and let it be
// archived under another platform's name. That is a green release that strands
// the fleet, and it is the same one-platform's-bytes-under-two-names failure
// the archive collision check exists to refuse.
func buildEnv(base []string, t Target) []string {
	env := make([]string, 0, len(base)+len(t.Env)+3)
	env = append(env, base...)
	env = append(env, "CGO_ENABLED=0")
	env = append(env, t.Env...)
	return append(env,
		"GOOS="+t.Goos,
		"GOARCH="+t.Goarch,
	)
}

// Options controls a run.
type Options struct {
	// Dir is the project root (where .goreleaser.yaml and dist/ live).
	Dir string
	// Builder compiles targets; nil means GoBuilder.
	Builder Builder
	// SkipBuild packages binaries that are already on disk.
	SkipBuild bool
	// Snapshot marks the run as unreleasable (no tag required).
	Snapshot bool
	// Fields carries the version/tag/commit the caller resolved from git.
	Fields Fields
}

// Run executes the T0 pipeline: build → archive → checksum, returning the
// artifact ledger. Every stage verifies its own output exists before the run
// is reported as successful.
func Run(ctx context.Context, cfg *Config, opts Options) (*Ledger, error) {
	if opts.Fields.Version == "" {
		return nil, fmt.Errorf("release: no version resolved (pass --snapshot or build from a tag)")
	}
	fields := opts.Fields
	fields.ProjectName = cfg.ProjectName

	plan, err := BuildPlan(cfg, fields)
	if err != nil {
		return nil, err
	}
	dist := filepath.Join(opts.Dir, cfg.Dist)
	if err := os.MkdirAll(dist, 0o755); err != nil {
		return nil, err
	}

	builder := opts.Builder
	if builder == nil {
		builder = GoBuilder{Stderr: os.Stderr}
	}
	for _, t := range plan.Targets {
		abs := t
		abs.Path = filepath.Join(opts.Dir, t.Path)
		if abs.Dir == "" {
			abs.Dir = opts.Dir
		} else if !filepath.IsAbs(abs.Dir) {
			abs.Dir = filepath.Join(opts.Dir, abs.Dir)
		}
		if !opts.SkipBuild {
			if err := builder.Build(ctx, abs); err != nil {
				return nil, err
			}
		}
		// Fail closed: a builder that reports success without producing the
		// binary must not reach the archive stage looking green.
		if _, err := os.Stat(abs.Path); err != nil {
			return nil, fmt.Errorf("release: build %s/%s reported success but produced no binary at %s", t.Goos, t.Goarch, t.Path)
		}
	}

	ledger := &Ledger{
		Schema:      LedgerSchema,
		ProjectName: cfg.ProjectName,
		Version:     fields.Version,
		Tag:         fields.Tag,
		Snapshot:    opts.Snapshot,
		Dist:        cfg.Dist,
	}
	for _, a := range plan.Archives {
		members := make([]Member, 0, len(a.Members))
		for _, m := range a.Members {
			m.Source = filepath.Join(opts.Dir, m.Source)
			members = append(members, m)
		}
		out := filepath.Join(opts.Dir, a.Path)
		if err := WriteArchive(out, a.Format, members); err != nil {
			return nil, err
		}
		art, err := describe(out, "archive")
		if err != nil {
			return nil, err
		}
		art.Goos, art.Goarch = a.Target.Goos, a.Target.Goarch
		ledger.Artifacts = append(ledger.Artifacts, art)
	}

	if plan.ChecksumName != "" {
		path := filepath.Join(dist, plan.ChecksumName)
		if err := writeChecksums(path, ledger.Artifacts); err != nil {
			return nil, err
		}
		art, err := describe(path, "checksum")
		if err != nil {
			return nil, err
		}
		ledger.Artifacts = append(ledger.Artifacts, art)
	}
	return ledger, nil
}

// writeChecksums emits sha256sum(1) manifest format: "<hex>  <name>\n",
// sorted by name so the file is byte-stable across runs.
func writeChecksums(path string, artifacts []Artifact) error {
	// Sort the artifacts by name, not the formatted lines by a byte offset:
	// the offset only happens to be the name because the digest is sha256.
	rows := make([]Artifact, 0, len(artifacts))
	for _, a := range artifacts {
		if a.Type != "archive" {
			continue
		}
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	var sb strings.Builder
	for _, a := range rows {
		fmt.Fprintf(&sb, "%s  %s\n", a.SHA256, a.Name)
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func describe(path, kind string) (Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return Artifact{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{
		Name:   filepath.Base(path),
		Path:   path,
		Type:   kind,
		SHA256: hex.EncodeToString(h.Sum(nil)),
		Size:   n,
	}, nil
}

// JSON renders the ledger.
func (l *Ledger) JSON() ([]byte, error) {
	return json.MarshalIndent(l, "", "  ")
}
