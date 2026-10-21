package dagrun

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/unit/frontmatter"
)

// RunKind is the `run.kind` of a command record: how long the work lives and
// whether a client attaches to it.
type RunKind string

const (
	RunOneShot     RunKind = "one-shot"
	RunInteractive RunKind = "interactive"
	RunLongRunning RunKind = "long-running"
	RunService     RunKind = "service"
)

func (k RunKind) known() bool {
	switch k {
	case RunOneShot, RunInteractive, RunLongRunning, RunService:
		return true
	}
	return false
}

// EgressMode is the `run.network.egress` of a command record.
type EgressMode string

const (
	EgressNone     EgressMode = "none"
	EgressPlane    EgressMode = "plane"
	EgressInternet EgressMode = "internet"
)

func (m EgressMode) known() bool {
	switch m {
	case EgressNone, EgressPlane, EgressInternet:
		return true
	}
	return false
}

// TargetName is the one target every run manifest has.
const TargetName = "run"

var (
	// ErrInvalidInput: Generate refused its input. Nothing was produced.
	ErrInvalidInput = errors.New("dagrun: invalid run input")
	// ErrInvalidManifest: the bytes are not a run manifest this package wrote.
	ErrInvalidManifest = errors.New("dagrun: invalid run manifest")
)

// CommandMeta is the command identity recorded in the manifest frontmatter.
type CommandMeta struct {
	Name   string `yaml:"name"`
	Digest string `yaml:"digest"`
}

// Port is one named service port (`run.ports`).
type Port struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
}

// ManifestMeta is what a run manifest carries under frontmatter `metadata:`:
// the facts dag has no target line for. The plain dag engine ignores them.
type ManifestMeta struct {
	RunKind     RunKind           `yaml:"run-kind"`
	Workspace   string            `yaml:"workspace"`
	RunID       string            `yaml:"run-id"`
	Command     CommandMeta       `yaml:"command"`
	Artifact    map[string]string `yaml:"artifact,omitempty"` // goos/goarch -> sha256:<hex>
	Cwd         string            `yaml:"cwd,omitempty"`
	Egress      EgressMode        `yaml:"egress"`
	EgressHosts []string          `yaml:"egress-hosts,omitempty"`
	Ports       []Port            `yaml:"ports,omitempty"`
	// Relays are the in-pod expose relays `bashy dks relay --all` starts
	// before the run's own command (see relay.go). Empty is the common case
	// and writes no frontmatter line at all.
	Relays []Relay `yaml:"relays,omitempty"`
}

// manifestFront is the whole frontmatter block, in the order it is written.
type manifestFront struct {
	Name        string       `yaml:"name"`
	Description string       `yaml:"description"`
	Type        string       `yaml:"type"`
	Metadata    ManifestMeta `yaml:"metadata"`
}

// Manifest is a parsed run manifest: the frontmatter facts plus the one
// `run` target as yoke/pkg/dag reads it.
type Manifest struct {
	Name string
	Meta ManifestMeta
	Task *dag.Task
	Text []byte // the exact bytes parsed
}

// ParseManifest reads a run manifest with the dag parser and checks the
// shape Generate writes: strict-YAML frontmatter of `type: dag`, exactly one
// `run` target, a known run kind and egress, label-safe workspace and run
// ids.
func ParseManifest(text []byte) (*Manifest, error) {
	block, _, err := frontmatter.Split(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	var front manifestFront
	if err := yaml.Unmarshal([]byte(block), &front); err != nil {
		return nil, fmt.Errorf("%w: frontmatter: %v", ErrInvalidManifest, err)
	}
	doc, err := dag.Parse(bytes.NewReader(text), "run.dag.md")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if doc.FrontmatterYAMLErr != "" {
		return nil, fmt.Errorf("%w: frontmatter is not strict YAML: %s", ErrInvalidManifest, doc.FrontmatterYAMLErr)
	}
	if front.Type != "dag" {
		return nil, fmt.Errorf("%w: type %q, want dag", ErrInvalidManifest, front.Type)
	}
	if len(doc.Tasks) != 1 || doc.Tasks[0].Name != TargetName {
		return nil, fmt.Errorf("%w: want exactly one %q target, have %v", ErrInvalidManifest, TargetName, doc.Order)
	}
	meta := front.Metadata
	if !meta.RunKind.known() {
		return nil, fmt.Errorf("%w: unknown run-kind %q", ErrInvalidManifest, meta.RunKind)
	}
	if !meta.Egress.known() {
		return nil, fmt.Errorf("%w: unknown egress %q", ErrInvalidManifest, meta.Egress)
	}
	if !labelValueRE.MatchString(meta.Workspace) {
		return nil, fmt.Errorf("%w: workspace %q is not label-safe", ErrInvalidManifest, meta.Workspace)
	}
	if !runIDRE.MatchString(meta.RunID) {
		return nil, fmt.Errorf("%w: run-id %q is not label-safe", ErrInvalidManifest, meta.RunID)
	}
	if strings.TrimSpace(meta.Command.Name) == "" {
		return nil, fmt.Errorf("%w: no command name", ErrInvalidManifest)
	}
	if err := ValidateRelays(meta.Relays); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	return &Manifest{Name: front.Name, Meta: meta, Task: doc.Tasks[0], Text: text}, nil
}

var (
	// labelValueRE is a Kubernetes label value: at most 63 characters,
	// alphanumeric at both ends, `-_.` inside.
	labelValueRE = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	// runIDRE also has to fit an object name once lowercased and prefixed.
	runIDRE = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9]{0,46}[A-Za-z0-9])?$`)
	// sha256RE is a pinned digest, bare or `sha256:`-prefixed.
	sha256RE   = regexp.MustCompile(`^(sha256:)?[0-9a-f]{64}$`)
	platformRE = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	envKeyRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// normalizeDigest returns the `sha256:<hex>` spelling of a pinned digest.
func normalizeDigest(d string) (string, bool) {
	if !sha256RE.MatchString(d) {
		return "", false
	}
	if strings.HasPrefix(d, "sha256:") {
		return d, true
	}
	return "sha256:" + d, true
}

// hasControl reports a control character (newline, CR, NUL, …): such a
// value could break a frontmatter or metadata line.
func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// isListToken is a value dag's list splitter keeps as one token: no
// whitespace, no comma, no control character, and nothing dag's metadata
// `${NAME}` expansion would rewrite.
func isListToken(s string) bool {
	return s != "" && !hasControl(s) && !strings.ContainsAny(s, " \t,") && !strings.Contains(s, "${")
}

// matchPlatform is the os/arch a Match: line pins ("" when it does not).
// Both the Kubernetes node-label spelling and dag's bare host-fact
// spelling are accepted.
func matchPlatform(m map[string]string) (goos, goarch string) {
	goos = m["kubernetes.io/os"]
	if goos == "" {
		goos = m["os"]
	}
	goarch = m["kubernetes.io/arch"]
	if goarch == "" {
		goarch = m["arch"]
	}
	return goos, goarch
}

// formatDuration is the Timeout: spelling: time.Duration's own, which
// time.ParseDuration (the dag parser) reads back exactly.
func formatDuration(d time.Duration) string { return d.String() }
