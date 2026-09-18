package weave

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/fleet"
)

// ToolProfile is the persistent profile + track record for one agentic CLI,
// stored at <weaveStateRoot>/tools/<tool>.json (system-wide, cross-queue). It is
// the conductor's source of truth for HOW to launch a tool unattended and how
// well it has performed per role. Bootstrapped by `weave fleet interview`;
// consumed by the conductor when assembling a `weave start` invocation.
type ToolProfile struct {
	Tool    string `json:"tool"`
	Bin     string `json:"bin,omitempty"`     // resolved path (from PATH)
	Version string `json:"version,omitempty"` // first line of `<tool> --version`

	// Launch contract — the headless/unattended invocation. The conductor runs:
	//   weave start --issue N … -- <tool> <HeadlessArgs...> "<body>"
	// i.e. HeadlessArgs are passed verbatim and the issue body is appended as the
	// final prompt argument. A BARE launch (no HeadlessArgs) hangs at the tool's
	// interactive/trust prompt — that is the failure this profile prevents.
	HeadlessArgs         []string `json:"headless_args"`
	TrustPreseed         string   `json:"trust_preseed,omitempty"` // e.g. ".claude.json" — config seed that suppresses first-run trust prompts
	TrustClear           string   `json:"trust_clear,omitempty"`   // e.g. "say:1" — how to clear a per-dir trust prompt
	SupportsSay          bool     `json:"supports_say"`            // mid-run `weave say` injection reaches it (steerable)
	SupportsGracefulQuit bool     `json:"supports_graceful_quit"`
	Notes                string   `json:"notes,omitempty"`

	// AuthHint is a human-facing instruction for authenticating the tool when
	// a readiness probe reports needs-login (e.g. "run `agy` once
	// interactively to sign in"). Empty for tools that authenticate via an
	// env var / API key with no interactive step.
	AuthHint string `json:"auth_hint,omitempty"`

	// Contract verification — set by `fleet interview --live`, which runs the
	// tool with its HeadlessArgs on a trivial prompt and watches for a flag-parse
	// error (the signature of a STALE contract: e.g. codex 0.141.0 renamed
	// --workspace to --sandbox and exits 2 with "unexpected argument"). This is
	// what turns a silent campaign-time failure into a caught preflight failure.
	ContractOK        *bool     `json:"contract_ok,omitempty"` // nil=unchecked, true=parses, false=STALE
	ContractNote      string    `json:"contract_note,omitempty"`
	ContractCheckedAt time.Time `json:"contract_checked_at,omitempty"`

	// Track record — accrued per role (conductor, coder, qa, doc, …).
	Roles         map[string]*RoleRecord `json:"roles,omitempty"`
	InterviewedAt time.Time              `json:"interviewed_at,omitempty"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

// RoleRecord is one tool's accrued performance in one role, plus a qualitative
// conductor/operator review (rating + one-line verdict + rationale notes).
type RoleRecord struct {
	Runs       int       `json:"runs"`
	Passed     int       `json:"passed"`
	Failed     int       `json:"failed"`
	Rating     int       `json:"rating,omitempty"`  // 0–5 suitability for this role
	Verdict    string    `json:"verdict,omitempty"` // one-line review
	Notes      []string  `json:"notes,omitempty"`   // rationale / outcome notes (capped at 10)
	ReviewedAt time.Time `json:"reviewed_at,omitempty"`
}

// setRoleReview records (or updates) a qualitative review of a tool's fitness
// for a role into its persistent profile — the system-wide track record a
// conductor consults when routing. Preserves accrued run counts.
func setRoleReview(dir, tool, role string, rating int, verdict string, notes []string, now time.Time) error {
	p, ok := loadToolProfile(dir, tool)
	if !ok {
		seed, _ := seededContract(tool)
		p = &seed
		p.Tool = tool
	}
	if p.Roles == nil {
		p.Roles = map[string]*RoleRecord{}
	}
	r := p.Roles[role]
	if r == nil {
		r = &RoleRecord{}
		p.Roles[role] = r
	}
	r.Rating, r.Verdict, r.ReviewedAt = rating, verdict, now
	for _, n := range notes {
		if n != "" {
			r.Notes = append(r.Notes, n)
		}
	}
	if len(r.Notes) > 10 {
		r.Notes = r.Notes[len(r.Notes)-10:]
	}
	return saveToolProfile(dir, p)
}

// seededContract is the known-good headless invocation for a tool, read from
// the fleet registry (coreutils/pkg/fleet).
//
// It used to be a Go map here, a second copy of the same launch flags that
// pkg/chat carried and the capability matrix scored against. The three drifted
// independently. Now a tool declares its contract once — `bashy tool show
// codex` is what weave launches — and `bashy tool set` changes it without a
// rebuild.
//
// The accrued track record (Roles, ContractOK, run counts) is NOT registry
// data: it is what this host learned, and it keeps living in the per-tool
// profile JSON. The registry supplies the seed; the profile accrues the
// evidence.
func seededContract(tool string) (ToolProfile, bool) {
	t, ok := fleetCatalog().Tool(tool)
	if !ok || !t.IsCLI() {
		return ToolProfile{}, false
	}
	args, ok := t.ArgvPrefix("")
	if !ok {
		return ToolProfile{}, false // recognized, but not drivable headless
	}
	l := t.CLI.Launch
	return ToolProfile{
		Tool:                 t.Name,
		HeadlessArgs:         args,
		TrustPreseed:         l.TrustPreseed,
		TrustClear:           l.TrustClear,
		SupportsSay:          l.SupportsSay,
		SupportsGracefulQuit: l.SupportsGracefulQuit,
		Notes:                l.Notes,
		AuthHint:             l.AuthHint,
	}, true
}

// fleetCatalog is indirected so tests can pin the registry.
var fleetCatalog = func() *fleet.Catalog { return fleet.New() }

func weaveToolsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(weaveStateRoot(home), "tools"), nil
}

func toolProfilePath(dir, tool string) string { return filepath.Join(dir, tool+".json") }

func loadToolProfile(dir, tool string) (*ToolProfile, bool) {
	b, err := os.ReadFile(toolProfilePath(dir, tool))
	if err != nil {
		return nil, false
	}
	var p ToolProfile
	if json.Unmarshal(b, &p) != nil {
		return nil, false
	}
	return &p, true
}

func saveToolProfile(dir string, p *ToolProfile) error {
	if err := ensureWeaveQueueDirPath(dir); err != nil {
		return err
	}
	p.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(toolProfilePath(dir, p.Tool), b, 0o644)
}

// interviewTool resolves a tool's binary + version and records its launch
// contract into a profile (seeding from the known contracts for a first
// interview, preserving any accrued track record). This is the lightweight
// one-on-one calibration: it makes the unattended launch contract explicit and
// discoverable. (A deeper live capability run — launch on a fixed micro-task and
// gate-verify the result — is the next layer, and `fleet tournament` will rank
// tools per role using the same RoleRecord.)
func interviewTool(dir, tool string, now time.Time, live bool) (*ToolProfile, error) {
	p, ok := loadToolProfile(dir, tool)
	if !ok {
		seed, _ := seededContract(tool) // zero value if unknown
		p = &seed
		p.Tool = tool
	}
	if path, err := exec.LookPath(tool); err == nil {
		p.Bin = path
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if out, verr := exec.CommandContext(ctx, path, "--version").CombinedOutput(); verr == nil {
			if line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]); line != "" {
				p.Version = line
			}
		}
		cancel()
	}
	if live && p.Bin != "" && len(p.HeadlessArgs) > 0 {
		okc, note := liveProbeContract(tool, p.HeadlessArgs, now)
		if !okc {
			// Self-heal: the recorded contract is stale, but the maintained seed
			// contract may already carry the fixed flags (codex --sandbox). Probe
			// the seed; if it parses, adopt it.
			if seed, has := seededContract(tool); has && !sameArgs(seed.HeadlessArgs, p.HeadlessArgs) {
				if sok, snote := liveProbeContract(tool, seed.HeadlessArgs, now); sok {
					p.HeadlessArgs = seed.HeadlessArgs
					if seed.Notes != "" {
						p.Notes = seed.Notes
					}
					okc, note = true, "auto-healed from seed (recorded contract was stale): "+snote
				}
			}
		}
		p.ContractOK = &okc
		p.ContractNote = note
		p.ContractCheckedAt = now
	}
	p.InterviewedAt = now
	if err := saveToolProfile(dir, p); err != nil {
		return nil, err
	}
	return p, nil
}

// contractErrSignatures are stderr/stdout markers that mean the tool REJECTED a
// flag — i.e. the recorded launch contract no longer matches the installed CLI.
var contractErrSignatures = []string{
	"unexpected argument", "unknown option", "unknown flag", "invalid option",
	"unrecognized option", "unrecognized argument", "Usage:", "USAGE:",
}

// liveProbeContract runs the tool with its headless contract on a trivial prompt
// and watches for a flag-parse error — the signature of a STALE launch contract
// (e.g. codex 0.141.0 renamed --workspace to --sandbox, exiting 2 with
// "unexpected argument"). A flag error surfaces in the first second; if the tool
// is still running at the probe deadline, the flags parsed fine. This turns a
// silent campaign-time failure into a caught preflight failure.
func liveProbeContract(tool string, args []string, now time.Time) (bool, string) {
	full := append(append([]string{}, args...), "Reply with exactly PROBE_OK and do nothing else.")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, full...)
	if d, err := os.MkdirTemp("", "weave-contract-probe-"); err == nil {
		cmd.Dir = d
		defer os.RemoveAll(d)
	}
	out, _ := cmd.CombinedOutput()
	s := string(out)
	for _, sig := range contractErrSignatures {
		if strings.Contains(s, sig) {
			return false, "STALE contract — tool rejected a flag (" + strings.TrimSpace(sig) + "); re-check its CLI args"
		}
	}
	if strings.Contains(s, "PROBE_OK") {
		return true, "verified end-to-end (flags parse + tool responded)"
	}
	if ctx.Err() == context.DeadlineExceeded {
		return true, "flags parse (tool ran past the 25s probe without a flag error)"
	}
	return true, "flags parse"
}

// authGateSignatures are stdout/stderr markers that mean the tool is blocked on
// an interactive AUTH or TRUST gate rather than doing work — the failure mode
// that silently stalls a headless fleet worker until its idle-timeout (agy
// "you are currently not signed in", codex/claude "do you trust this
// directory?"). Matched case-insensitively.
var authGateSignatures = []string{
	"not signed in", "sign in", "sign-in", "please log in", "please login",
	"not logged in", "log in to", "login required", "authentication required",
	"authenticate", "unauthorized", "run `login`", "run 'login'",
	"do you trust", "trust the contents", "trust this", "requires permission",
	"you must log in", "session expired", "no api key", "api key not",
}

// ReadyStatus values from liveProbeReady.
const (
	ReadyOK        = "ready"          // ran headless and responded / parsed fine
	ReadyNeedsAuth = "needs-login"    // blocked on an auth/trust gate
	ReadyStale     = "stale-contract" // rejected a launch flag (contract drift)
)

// liveProbeReady runs the tool headless on a trivial prompt and classifies
// whether it is actually READY to be dispatched, distinguishing a working tool
// from one silently blocked on an auth/trust gate. It is the readiness check
// `fleet --auth` and a conductor pre-flight use so an unauthenticated tool
// (installed and "available" on PATH) is caught before it stalls a real run.
func liveProbeReady(tool string, args []string) (status, note string) {
	full := append(append([]string{}, args...), "Reply with exactly PROBE_OK and do nothing else.")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, full...)
	if d, err := os.MkdirTemp("", "weave-ready-probe-"); err == nil {
		cmd.Dir = d
		defer os.RemoveAll(d)
	}
	out, _ := cmd.CombinedOutput()
	return classifyReadyOutput(string(out), ctx.Err() == context.DeadlineExceeded)
}

// classifyReadyOutput turns a readiness-probe's combined output (and whether it
// hit the deadline) into a ReadyStatus. Split out from liveProbeReady so the
// classification is unit-testable without launching a real tool.
func classifyReadyOutput(raw string, timedOut bool) (status, note string) {
	// One classifier for the whole fleet (pkg/agentctl). weave's probe launches a
	// bare TOOL, so it can never see a bad model id — that is what
	// `agents verify --live` is for, and both now read the same signatures rather
	// than drifting apart into two opinions about what "not signed in" means.
	st, note := agentctl.Classify(raw, timedOut)
	switch st {
	case agentctl.ProbeStaleContract:
		return ReadyStale, note
	case agentctl.ProbeNeedsAuth:
		return ReadyNeedsAuth, note
	case agentctl.ProbeBadModel:
		return ReadyStale, note
	default:
		return ReadyOK, note
	}
}

// recordToolOutcome appends a role outcome to a tool's track record (used by the
// conductor at converge time, and by `fleet tournament`).
func recordToolOutcome(dir, tool, role string, passed bool, note string) error {
	p, ok := loadToolProfile(dir, tool)
	if !ok {
		seed, _ := seededContract(tool)
		p = &seed
		p.Tool = tool
	}
	if p.Roles == nil {
		p.Roles = map[string]*RoleRecord{}
	}
	r := p.Roles[role]
	if r == nil {
		r = &RoleRecord{}
		p.Roles[role] = r
	}
	r.Runs++
	if passed {
		r.Passed++
	} else {
		r.Failed++
	}
	if note != "" {
		r.Notes = append(r.Notes, note)
		if len(r.Notes) > 10 {
			r.Notes = r.Notes[len(r.Notes)-10:]
		}
	}
	return saveToolProfile(dir, p)
}

func sortedRoleNames(p *ToolProfile) []string {
	names := make([]string, 0, len(p.Roles))
	for k := range p.Roles {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func sameArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
