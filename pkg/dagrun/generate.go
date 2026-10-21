package dagrun

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/yoke/pkg/dag"
)

// CommandRef is the identity of the command being run: the record digest
// plus one pinned artifact sha256 per goos/goarch. It is a plain copy of the
// command record's fields, so this package does not depend on the registry
// types. A command shipped with bashy also names the bashy release and, when
// a Linux executor runs it from the bashy-built image (nothing fetched), that
// image; both are optional and recorded in the manifest description.
type CommandRef struct {
	Name      string
	Digest    string            // record digest, sha256
	Artifacts map[string]string // "goos/goarch" -> sha256
	Release   string            // shipped only: bashy "<version> <commit>"; "" otherwise
	Image     string            // shipped only: the executor image carrying it; "" otherwise
}

// description is the manifest's description line: the command, and for a
// shipped command the release and image that are its identity.
func (c CommandRef) description() (string, error) {
	if hasControl(c.Release) || hasControl(c.Image) {
		return "", invalid("command %s: release %q or image %q contains a control character", c.Name, c.Release, c.Image)
	}
	var shipped []string
	if c.Release != "" {
		shipped = append(shipped, "bashy release "+c.Release)
	}
	if c.Image != "" {
		shipped = append(shipped, "image "+c.Image)
	}
	id := ""
	if len(shipped) > 0 {
		id = " (shipped: " + strings.Join(shipped, ", ") + ")"
	}
	return "bashy run manifest for " + c.Name + id + " (generated; do not edit)", nil
}

// Resources is `run.resources`.
type Resources struct {
	CPU    int    // whole CPUs; 0 = no request
	Memory string // dag Memory: spelling, e.g. 4Gi; "" = no request
}

// Accelerator is `run.accelerator`; Memory is per device, never aggregate.
type Accelerator struct {
	Kind   string // e.g. gpu; required once anything else is set
	Family string // e.g. cuda
	Count  int
	Memory string // dag Accelerator-Memory: spelling, e.g. 16Gi
}

func (a Accelerator) empty() bool { return a == Accelerator{} }

// Egress is `run.network`.
type Egress struct {
	Mode  EgressMode // "" = none
	Hosts []string   // optional allow-list (plane / internet only)
}

// RunSpec is the command record's additive `run:` block. The zero value is
// the record with no `run:` block: one-shot, single, no requests, egress none.
type RunSpec struct {
	Kind         RunKind
	Resources    Resources
	Accelerator  Accelerator
	Ports        []Port   // service only
	Credentials  []string // vault references, never values
	Egress       Egress
	Distribution string // dag Distribution; "" = single
	Timeout      time.Duration
	Outputs      []string // dag Artifacts: checked after exit
}

// relaysFor copies the in-pod relay list into the manifest in NAME order,
// so the same set of exposes always writes the same frontmatter. Generate
// stays pure: the ports are the caller's deterministic plan, not an
// allocation made here.
func relaysFor(rs []Relay) ([]Relay, error) {
	if len(rs) == 0 {
		return nil, nil
	}
	out := append([]Relay(nil), rs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if err := ValidateRelays(out); err != nil {
		return nil, invalid("%v", err)
	}
	return out, nil
}

// GenerateInput is everything Generate reads: the command record, the argv
// typed at the session shell, and the context it ran in. The caller supplies
// the run id; Generate reads no clock and no randomness.
type GenerateInput struct {
	Command     CommandRef
	Argv        []string
	WorkspaceID string
	RunID       string
	Cwd         string            // logical cwd, e.g. ~/docs
	Env         map[string]string // EXPORTED env only: the only env that travels
	Venue       string            // dag venue (cluster, native, sandbox, …)
	Match       map[string]string // host capability labels (os/arch/host/backend)
	Run         RunSpec
	// Relays are the in-pod expose relays the run wraps its command with.
	// The caller plans the loopback ports; Generate only records them.
	Relays []Relay
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, a...))
}

// Generate renders a run manifest: an ordinary bashy dag document with one
// `run` target. It is pure and deterministic — every map is written in sorted
// key order — so the same input gives byte-identical output. It fails closed
// (ErrInvalidInput, nothing produced) on anything it cannot carry exactly,
// and only returns text the dag parser reads back and
// TaskSpec.ValidateForPipeline accepts.
func Generate(in GenerateInput) ([]byte, error) {
	body, err := quoteArgv(in.Argv)
	if err != nil {
		return nil, err
	}
	meta, err := generateMeta(in)
	if err != nil {
		return nil, err
	}
	target, err := generateTarget(in, meta.RunKind)
	if err != nil {
		return nil, err
	}
	desc, err := in.Command.description()
	if err != nil {
		return nil, err
	}

	var fm bytes.Buffer
	enc := yaml.NewEncoder(&fm)
	enc.SetIndent(2)
	if err := enc.Encode(manifestFront{
		Name:        "run-" + strings.ToLower(in.RunID),
		Description: desc,
		Type:        "dag",
		Metadata:    meta,
	}); err != nil {
		return nil, invalid("encode frontmatter: %v", err)
	}
	if err := enc.Close(); err != nil {
		return nil, invalid("encode frontmatter: %v", err)
	}

	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(fm.Bytes())
	out.WriteString("---\n## " + TargetName + "\n")
	for _, ln := range target {
		out.WriteString(ln + "\n")
	}
	out.WriteString("```sh\n" + body + "\n```\n")

	if err := checkGenerated(out.Bytes(), in); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// quoteArgv re-quotes argv with the shell's own quoting (bash dialect: the
// dag `sh` body runs under the bash interpreter). Non-printable bytes,
// newlines included, come out as $'…' escapes, so the body is always one
// line and can never close its own code fence.
func quoteArgv(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", invalid("empty argv")
	}
	words := make([]string, len(argv))
	for i, a := range argv {
		q, err := syntax.Quote(a, syntax.LangBash)
		if err != nil {
			return "", invalid("argv[%d] is not representable in a shell word: %v", i, err)
		}
		words[i] = q
	}
	body := strings.Join(words, " ")
	if strings.ContainsAny(body, "\r\n") {
		return "", invalid("argv quoting produced a multi-line body")
	}
	return body, nil
}

func generateMeta(in GenerateInput) (ManifestMeta, error) {
	var m ManifestMeta
	m.RunKind = in.Run.Kind
	if m.RunKind == "" {
		m.RunKind = RunOneShot
	}
	if !m.RunKind.known() {
		return m, invalid("unknown run kind %q", in.Run.Kind)
	}
	if !labelValueRE.MatchString(in.WorkspaceID) {
		return m, invalid("workspace id %q is not label-safe", in.WorkspaceID)
	}
	if !runIDRE.MatchString(in.RunID) {
		return m, invalid("run id %q is not label-safe (alphanumeric and '-', at most 48)", in.RunID)
	}
	m.Workspace, m.RunID = in.WorkspaceID, in.RunID

	if strings.TrimSpace(in.Command.Name) == "" || hasControl(in.Command.Name) {
		return m, invalid("command name %q", in.Command.Name)
	}
	digest, ok := normalizeDigest(in.Command.Digest)
	if !ok {
		return m, invalid("command %s: record digest %q is not a sha256", in.Command.Name, in.Command.Digest)
	}
	m.Command = CommandMeta{Name: in.Command.Name, Digest: digest}
	for platform, d := range in.Command.Artifacts {
		if !platformRE.MatchString(platform) {
			return m, invalid("command %s: artifact platform %q is not goos/goarch", in.Command.Name, platform)
		}
		nd, ok := normalizeDigest(d)
		if !ok {
			return m, invalid("command %s: artifact %s digest %q is not a sha256", in.Command.Name, platform, d)
		}
		if m.Artifact == nil {
			m.Artifact = map[string]string{}
		}
		m.Artifact[platform] = nd
	}
	// No digest for a pinned platform = ineligible: portability comes from
	// real per-platform artifacts, never from the bashy interface.
	if goos, goarch := matchPlatform(in.Match); goos != "" || goarch != "" {
		found := false
		for platform := range m.Artifact {
			o, a, _ := strings.Cut(platform, "/")
			if (goos == "" || o == goos) && (goarch == "" || a == goarch) {
				found = true
				break
			}
		}
		if !found {
			return m, invalid("command %s has no artifact digest for the pinned platform %s/%s",
				in.Command.Name, orAny(goos), orAny(goarch))
		}
	}

	if hasControl(in.Cwd) {
		return m, invalid("cwd %q contains a control character", in.Cwd)
	}
	m.Cwd = in.Cwd

	m.Egress = in.Run.Egress.Mode
	if m.Egress == "" {
		m.Egress = EgressNone
	}
	if !m.Egress.known() {
		return m, invalid("unknown egress %q", in.Run.Egress.Mode)
	}
	if m.Egress == EgressNone && len(in.Run.Egress.Hosts) > 0 {
		return m, invalid("egress none takes no hosts allow-list")
	}
	for _, h := range in.Run.Egress.Hosts {
		if !isListToken(h) {
			return m, invalid("egress host %q", h)
		}
	}
	m.EgressHosts = sortedCopy(in.Run.Egress.Hosts)

	if len(in.Run.Ports) > 0 && m.RunKind != RunService {
		return m, invalid("ports are for run kind service, not %s", m.RunKind)
	}
	for _, p := range in.Run.Ports {
		if !labelValueRE.MatchString(p.Name) || len(p.Name) > 15 || p.Port < 1 || p.Port > 65535 {
			return m, invalid("port %q %d", p.Name, p.Port)
		}
	}
	if len(in.Run.Ports) > 0 {
		m.Ports = append([]Port(nil), in.Run.Ports...)
		sort.Slice(m.Ports, func(i, j int) bool { return m.Ports[i].Name < m.Ports[j].Name })
	}
	relays, err := relaysFor(in.Relays)
	if err != nil {
		return m, err
	}
	m.Relays = relays
	return m, nil
}

// generateTarget writes the `run` target's metadata lines, each an existing
// dag metadata key, in a fixed order.
func generateTarget(in GenerateInput, kind RunKind) ([]string, error) {
	if strings.TrimSpace(in.Venue) == "" {
		return nil, invalid("no venue")
	}
	if !isListToken(in.Venue) {
		return nil, invalid("venue %q", in.Venue)
	}
	dist := in.Run.Distribution
	if dist == "" {
		dist = string(dag.DistributionSingle)
	}
	if !isListToken(dist) {
		return nil, invalid("distribution %q", dist)
	}
	lines := []string{"Venue: " + in.Venue, "Distribution: " + dist}

	if len(in.Match) > 0 {
		var pairs []string
		for _, k := range sortedKeys(in.Match) {
			v := in.Match[k]
			if !isListToken(k) || !isListToken(v) || strings.Contains(k, "=") {
				return nil, invalid("match %q=%q", k, v)
			}
			pairs = append(pairs, k+"="+v)
		}
		lines = append(lines, "Match: "+strings.Join(pairs, " "))
	}

	r := in.Run.Resources
	if r.CPU < 0 {
		return nil, invalid("negative cpu %d", r.CPU)
	}
	if r.CPU > 0 {
		lines = append(lines, "CPUPerTask: "+strconv.Itoa(r.CPU))
	}
	if r.Memory != "" {
		if !isListToken(r.Memory) {
			return nil, invalid("memory %q", r.Memory)
		}
		lines = append(lines, "Memory: "+r.Memory)
	}

	if a := in.Run.Accelerator; !a.empty() {
		for _, v := range []string{a.Kind, a.Family, a.Memory} {
			if v != "" && !isListToken(v) {
				return nil, invalid("accelerator %+v", a)
			}
		}
		if a.Kind != "" {
			lines = append(lines, "Accelerator: "+a.Kind)
		}
		if a.Family != "" {
			lines = append(lines, "Accelerator-Family: "+a.Family)
		}
		if a.Count != 0 {
			lines = append(lines, "Accelerator-Count: "+strconv.Itoa(a.Count))
		}
		if a.Memory != "" {
			lines = append(lines, "Accelerator-Memory: "+a.Memory)
		}
	}

	if len(in.Run.Credentials) > 0 {
		for _, c := range in.Run.Credentials {
			if !isListToken(c) {
				return nil, invalid("credential reference %q", c)
			}
		}
		lines = append(lines, "Secrets: "+strings.Join(sortedCopy(in.Run.Credentials), " "))
	}

	if len(in.Env) > 0 {
		var kv []string
		for _, k := range sortedKeys(in.Env) {
			v := in.Env[k]
			if !envKeyRE.MatchString(k) {
				return nil, invalid("env name %q", k)
			}
			if v != "" && !isListToken(v) {
				return nil, invalid("env %s: value is not representable on an Env: line (whitespace, comma, control or ${)", k)
			}
			kv = append(kv, k+"="+v)
		}
		lines = append(lines, "Env: "+strings.Join(kv, " "))
	}

	if in.Run.Timeout < 0 {
		return nil, invalid("negative timeout %v", in.Run.Timeout)
	}
	if in.Run.Timeout > 0 {
		lines = append(lines, "Timeout: "+formatDuration(in.Run.Timeout))
	}
	// Retries are always 0: executor loss never replays a command.
	lines = append(lines, "Retries: 0")

	if len(in.Run.Outputs) > 0 {
		for _, o := range in.Run.Outputs {
			if !isListToken(o) {
				return nil, invalid("output %q", o)
			}
		}
		lines = append(lines, "Artifacts: "+strings.Join(in.Run.Outputs, " "))
	}
	return lines, nil
}

// checkGenerated reads the generated text back the way every consumer will
// — dag.Parse, then the strict pipeline TaskSpec — so Generate never hands
// out a manifest the dag side would reject or read differently.
func checkGenerated(text []byte, in GenerateInput) error {
	m, err := ParseManifest(text)
	if err != nil {
		return invalid("generated manifest does not read back: %v", err)
	}
	spec := dag.SpecFor(m.Task)
	if err := spec.ValidateForPipeline(); err != nil {
		return invalid("%v", err)
	}
	if spec.Venue != in.Venue {
		return invalid("venue %q read back as %q", in.Venue, spec.Venue)
	}
	if in.Run.Resources.Memory != "" && spec.MemPerTask == 0 {
		return invalid("memory %q is not a dag memory quantity", in.Run.Resources.Memory)
	}
	if len(m.Meta.Relays) != len(in.Relays) {
		return invalid("generated relays do not read back one-for-one")
	}
	if len(m.Task.Env) != len(in.Env) || len(m.Task.Match) != len(in.Match) ||
		len(m.Task.Secrets) != len(in.Run.Credentials) || len(m.Task.Artifacts) != len(in.Run.Outputs) {
		return invalid("generated metadata does not read back one-for-one")
	}
	return nil
}

func orAny(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedCopy(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}
