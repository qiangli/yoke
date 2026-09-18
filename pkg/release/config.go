// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package release is the T0 core of `bashy release`: an original, narrow
// implementation of the build/archive/checksum stages over a subset of the
// .goreleaser.yaml schema.
//
// Why not import github.com/goreleaser/goreleaser/v2? The measurement of
// record (../../docs/bashy-release-pipeline-design.md §4) rules the whole CLI
// out: 77.3 MB, 277 new linked modules, 6 new MPL-2.0 deps and one module with
// no LICENSE file. The permissive core (pkg/{config,archive,context}) passes
// the gate at +6.7 MB / 39 new modules, but the stages a T0 slice actually
// needs — build matrix, deterministic archive, checksum file — are all in
// internal/pipe/* and are NOT importable at any price. So the schema subset
// below is read with the one YAML library coreutils already links, and the
// stages are written here: zero new modules, zero new license exposure.
//
// The tail (goreleaser itself for unembedded stages, cosign, syft, gpg,
// docker, upx) stays binmgr-managed external binaries, per
// docs/external-binary-builtins.md. That is the same split upstream
// GoReleaser itself uses.
//
// Everything here FAILS CLOSED: a config key naming a stage this tier does not
// implement is an error naming the stage, never a silent omission from the
// output. A release that quietly ships fewer assets than the config declares
// is exactly the "success state reached by the absence of evidence" the fleet
// evidence invariant forbids.
package release

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the supported subset of .goreleaser.yaml. Field names match
// upstream so an existing config parses unchanged, within the subset.
type Config struct {
	Version     int       `yaml:"version"`
	ProjectName string    `yaml:"project_name"`
	Builds      []Build   `yaml:"builds"`
	Archives    []Archive `yaml:"archives"`
	Checksum    Checksum  `yaml:"checksum"`
	Snapshot    Snapshot  `yaml:"snapshot"`
	Dist        string    `yaml:"dist"`

	// Stages this tier does not implement. They are declared so an existing
	// config produces a REFUSAL naming the stage, rather than a
	// known-fields parse error that reads like a typo, and never a release
	// that silently drops assets the config asked for.
	Dockers    yaml.Node `yaml:"dockers"`
	NFPMs      yaml.Node `yaml:"nfpms"`
	Brews      yaml.Node `yaml:"brews"`
	Homebrew   yaml.Node `yaml:"homebrew_casks"`
	Signs      yaml.Node `yaml:"signs"`
	DockerSign yaml.Node `yaml:"docker_signs"`
	SBOMs      yaml.Node `yaml:"sboms"`
	Publishers yaml.Node `yaml:"publishers"`
	Announce   yaml.Node `yaml:"announce"`
	Blobs      yaml.Node `yaml:"blobs"`
	Release    yaml.Node `yaml:"release"`
	Changelog  yaml.Node `yaml:"changelog"`
	UPX        yaml.Node `yaml:"upx"`
	Notarize   yaml.Node `yaml:"notarize"`
	Before     yaml.Node `yaml:"before"`
	After      yaml.Node `yaml:"after"`
}

// Build is one build matrix entry.
type Build struct {
	ID     string   `yaml:"id"`
	Main   string   `yaml:"main"`
	Binary string   `yaml:"binary"`
	Dir    string   `yaml:"dir"`
	Env    []string `yaml:"env"`
	Flags  []string `yaml:"flags"`
	// Tags are Go build tags, joined with commas into one -tags argument.
	// Kept separate from Flags because a project whose binary is only correct
	// under a tag set (ycode: sqlite,sqlite_unlock_notify,bindata) would
	// otherwise have to smuggle them through Flags, where a stray split would
	// silently produce a binary that builds and does not work.
	Tags    []string `yaml:"tags"`
	Ldflags []string `yaml:"ldflags"`
	Goos    []string `yaml:"goos"`
	Goarch  []string `yaml:"goarch"`
	Targets []string `yaml:"targets"`
	Ignore  []Ignore `yaml:"ignore"`
	Skip    bool     `yaml:"skip"`

	Hooks yaml.Node `yaml:"hooks"`
}

// Ignore excludes one goos/goarch pair from a build's matrix.
type Ignore struct {
	Goos   string `yaml:"goos"`
	Goarch string `yaml:"goarch"`
}

// Archive describes how built binaries are packaged.
type Archive struct {
	ID              string           `yaml:"id"`
	IDs             []string         `yaml:"ids"`
	Formats         []string         `yaml:"formats"`
	NameTemplate    string           `yaml:"name_template"`
	WrapInDirectory bool             `yaml:"wrap_in_directory"`
	Files           []string         `yaml:"files"`
	FormatOverrides []FormatOverride `yaml:"format_overrides"`
}

// FormatOverride selects a different archive format for one GOOS.
type FormatOverride struct {
	Goos    string   `yaml:"goos"`
	Formats []string `yaml:"formats"`
}

// Checksum describes the checksum manifest.
type Checksum struct {
	NameTemplate string `yaml:"name_template"`
	Algorithm    string `yaml:"algorithm"`
	Disable      bool   `yaml:"disable"`
}

// Snapshot carries the version used when there is no tag.
type Snapshot struct {
	VersionTemplate string `yaml:"version_template"`
}

// ErrUnsupportedStage reports a config key naming a stage this tier does not
// implement. Callers can test for it with errors.Is.
var ErrUnsupportedStage = errors.New("release: unsupported stage")

// refusal names each unimplemented stage and where the capability lives
// instead, so the error tells an operator what to do rather than only what
// failed.
type refusal struct {
	key  string
	node *yaml.Node
	hint string
}

const binmgrHint = "run the tail through the binmgr-managed goreleaser binary"

// LoadConfig reads and validates a .goreleaser.yaml subset from path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig parses and validates config bytes. Unknown keys are an error:
// a key this tier does not know is a stage it would silently skip.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := cfg.checkSupported(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) checkSupported() error {
	refusals := []refusal{
		{"dockers", &c.Dockers, binmgrHint},
		{"nfpms", &c.NFPMs, binmgrHint},
		{"brews", &c.Brews, binmgrHint},
		{"homebrew_casks", &c.Homebrew, binmgrHint},
		{"signs", &c.Signs, "cosign/gpg are binmgr-managed externals"},
		{"docker_signs", &c.DockerSign, "cosign is a binmgr-managed external"},
		{"sboms", &c.SBOMs, "syft is a binmgr-managed external"},
		{"upx", &c.UPX, "upx is a binmgr-managed external"},
		{"notarize", &c.Notarize, "notarization is a Tier 3 item"},
		{"publishers", &c.Publishers, binmgrHint},
		{"announce", &c.Announce, "refused by design: no announce integrations"},
		{"blobs", &c.Blobs, "refused by design: blob storage drags in cloud SDKs"},
		{"release", &c.Release, "publishing is Tier 1 (release publish)"},
		{"changelog", &c.Changelog, "changelog is Tier 2"},
		{"before", &c.Before, "global hooks are not implemented"},
		{"after", &c.After, "global hooks are not implemented"},
	}
	var named []string
	for _, r := range refusals {
		if r.node.IsZero() {
			continue
		}
		named = append(named, fmt.Sprintf("%s (%s)", r.key, r.hint))
	}
	for _, b := range c.Builds {
		if !b.Hooks.IsZero() {
			named = append(named, fmt.Sprintf("builds[%s].hooks (build hooks are not implemented)", b.ID))
		}
	}
	if len(named) > 0 {
		sort.Strings(named)
		return fmt.Errorf("%w: %s", ErrUnsupportedStage, strings.Join(named, ", "))
	}
	return nil
}

// defaultGoos/defaultGoarch mirror goreleaser's own build defaults.
var (
	defaultGoos   = []string{"darwin", "linux", "windows"}
	defaultGoarch = []string{"amd64", "arm64"}
)

// ApplyDefaults fills in the fields goreleaser defaults, so a minimal config
// behaves the same here. dir is used only to derive a missing project_name.
func (c *Config) ApplyDefaults(dir string) error {
	if c.ProjectName == "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		c.ProjectName = filepath.Base(abs)
	}
	if c.Dist == "" {
		c.Dist = "dist"
	}
	if len(c.Builds) == 0 {
		c.Builds = []Build{{}}
	}
	for i := range c.Builds {
		b := &c.Builds[i]
		if b.ID == "" {
			if len(c.Builds) == 1 {
				b.ID = "default"
			} else {
				return fmt.Errorf("release: builds[%d] has no id (required when there is more than one build)", i)
			}
		}
		if b.Main == "" {
			b.Main = "."
		}
		if b.Binary == "" {
			b.Binary = c.ProjectName
		}
		if len(b.Targets) == 0 {
			if len(b.Goos) == 0 {
				b.Goos = append([]string(nil), defaultGoos...)
			}
			if len(b.Goarch) == 0 {
				b.Goarch = append([]string(nil), defaultGoarch...)
			}
		}
	}
	if len(c.Archives) == 0 {
		c.Archives = []Archive{{}}
	}
	for i := range c.Archives {
		a := &c.Archives[i]
		if a.ID == "" {
			a.ID = "default"
		}
		if len(a.Formats) == 0 {
			a.Formats = []string{"tar.gz"}
		}
		if a.NameTemplate == "" {
			a.NameTemplate = "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
		}
	}
	if c.Checksum.Algorithm == "" {
		c.Checksum.Algorithm = "sha256"
	}
	if c.Checksum.NameTemplate == "" {
		c.Checksum.NameTemplate = "{{ .ProjectName }}_{{ .Version }}_checksums.txt"
	}
	if c.Snapshot.VersionTemplate == "" {
		c.Snapshot.VersionTemplate = "{{ .Version }}-SNAPSHOT-{{ .ShortCommit }}"
	}
	if c.Checksum.Algorithm != "sha256" {
		return fmt.Errorf("%w: checksum.algorithm=%q (only sha256 is implemented)", ErrUnsupportedStage, c.Checksum.Algorithm)
	}
	return nil
}
