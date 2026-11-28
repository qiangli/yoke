// Package verify is the trusted verifier behind every shaping verdict.
//
// A candidate never gets to describe itself. Three rules make that true.
//
// Frozen inputs. The contract and the required checks are read from the
// BASE commit — the version the principal is running today — not from the
// candidate. A candidate that edits a base check runs the base's bytes
// anyway; a candidate that ADDS a check gets it run in addition, never
// instead. Weakening a check is therefore not a way to pass, and adding one
// is always allowed.
//
// Spec changes. A candidate that edits or removes a base check changes the
// spec: it is refused unless the caller supplies the owner's review of
// exactly that spec (Options.Spec), and then it is graded against its own
// checks under the base's effect cap. An undo or restore grades the recorded
// version against the spec it carries (VersionSpec).
//
// Contained execution. Checks run in an isolated clone detached at the
// candidate commit, through pkg/shape/contain: cwd-only writes, scrubbed
// environment, loopback-only network, the base contract's effect cap, and
// resource limits. The live checkout is never the thing under test.
//
// Accounted execution. Every required check yields exactly one of
// pass/fail/skip/timeout/cancel and only all-pass passes — a skipped check
// is unproven, not lenient. On top of that the interpreter records each
// contracted-function invocation, and a contracted function the check set
// never invoked fails the verdict. Passing by not running the contract is
// the failure mode the accounting exists for.
//
// The result is a sealed record in pkg/shape/evidence, bound to exactly the
// inputs that produced it. The verifier is the only writer of those
// records.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/qiangli/yoke/pkg/shape/selfbin"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/gomod"
	"github.com/qiangli/yoke/pkg/policy/advice"
	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/shape/evidence"
)

// engineVersion changes when the verifier's own semantics change. It is
// part of the build id, so every record minted by an older engine goes
// stale rather than being honoured by a verifier that would now judge
// differently.
const engineVersion = "shape-verify/3"

// StagedChecks is the verifier-owned directory, inside the clone, that the
// frozen check bodies are written to. It is not a candidate path: whatever
// the candidate has at this location is replaced before anything runs.
const StagedChecks = ".shape/checks"

// callLogRel is the call log's path inside the clone. The log has to live
// in the clone because that is the only place a contained run may write.
const callLogRel = ".shape/calls.log"

// SkipExit is the exit status a check uses to report itself inapplicable.
// It is reported as a skip, and a skip never passes a verdict.
const SkipExit = 77

// Options configures one verification run.
type Options struct {
	// Home is the bashy home whose src/.evidence receives the record.
	Home string
	// RepoDir is the repository holding both commits. It is cloned; it is
	// never checked out, written to, or otherwise touched.
	RepoDir string
	// BaseSHA is the commit the contract and checks are frozen from.
	BaseSHA string
	// CandidateSHA is the commit under test.
	CandidateSHA string
	// Principal is who the verdict is for.
	Principal string
	// Reader reads a contract from a tree; UnitReader{} by default.
	Reader ContractReader
	// Runner executes one frozen check; TrustedRunner{} by default.
	Runner Runner
	// Timeout caps one check; 2 minutes by default.
	Timeout time.Duration
	// Contract lists surface elements (Surface keys, e.g.
	// "property:title") the user explicitly approved dropping in this
	// separate contract step. It is supplied by the caller, never read
	// from the candidate, so a candidate cannot approve its own removals.
	Contract []string
	// Spec authorizes grading a candidate that changes the spec (edits or
	// removes a base check) against its own spec. Empty refuses such a
	// candidate. A contract digest is an owner review of exactly that
	// candidate spec. VersionSpec grades a version already recorded on the
	// principal's line against the spec it carries (undo and restore). Like
	// Contract it is supplied by the caller, never read from the candidate.
	Spec string
	// KeepClone leaves the isolated clone on disk for inspection.
	KeepClone bool
}

// Status is one check's outcome. Only StatusPass contributes to a pass.
type Status = string

// CheckRun is one frozen check as handed to a runner.
type CheckRun struct {
	// Name is the check's tree-relative name, as frozen.
	Name string
	// Dir is the isolated clone, detached at the candidate commit. It is
	// the working directory and the only writable location.
	Dir string
	// Script is the absolute path of the frozen check body inside Dir.
	Script string
	// Cap is the effect cap spec from the BASE contract.
	Cap string
	// CallLog is the absolute path the interpreter appends contracted
	// function invocations to.
	CallLog string
	// Timeout caps this check.
	Timeout time.Duration
}

// Outcome is what a runner reports for one check.
type Outcome struct {
	Status Status
	// Calls are the contracted functions the interpreter observed. A
	// runner that drives the interpreter in-process reports them here; the
	// out-of-process runner reads them from the call log.
	Calls  []string
	Detail string
}

// Runner executes one frozen check inside the candidate clone. It is an
// interface so a host that owns the interpreter in-process can report
// invocations directly instead of through the call log — which is also the
// stronger arrangement, since an in-process recorder is not a file the
// contained run can reach.
type Runner interface {
	RunCheck(ctx context.Context, c CheckRun) (Outcome, error)
}

// Report is one verification run's result, alongside the record it sealed.
type Report struct {
	Record evidence.Record
	// Path is where the sealed record was written.
	Path string
	// Clone is the isolated clone directory; removed unless KeepClone.
	Clone string
	// Base and Candidate are the contracts as read from each commit.
	Base      Contract
	Candidate Contract
}

// Passed reports whether the run authorizes a merge or an activation.
func (r Report) Passed() bool { return r.Record.Verdict == evidence.VerdictPass }

// BuildID identifies this verifier build. A record minted by a different
// build is stale: a verifier that may judge differently has not judged
// this candidate.
func BuildID() string {
	rev := "devel"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				rev = s.Value
				break
			}
		}
		if rev == "devel" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			rev = info.Main.Version
		}
	}
	return engineVersion + "+" + rev
}

// Run verifies CandidateSHA against the contract frozen at BaseSHA and
// seals the verdict. It returns a Report for a fail as well as a pass: a
// recorded failure is the useful artefact, and the gate refuses it by
// verdict rather than by the absence of a record.
func Run(ctx context.Context, o Options) (Report, error) {
	if strings.TrimSpace(o.Home) == "" {
		return Report{}, errors.New("verify: a bashy home is required")
	}
	if o.RepoDir == "" || o.BaseSHA == "" || o.CandidateSHA == "" {
		return Report{}, errors.New("verify: repo dir, base and candidate are required")
	}
	if strings.TrimSpace(o.Principal) == "" {
		// Fail closed: a verdict with no-one to be for authorizes no-one.
		return Report{}, errors.New("verify: a principal is required")
	}
	reader := o.Reader
	if reader == nil {
		reader = UnitReader{}
	}
	runner := o.Runner
	if runner == nil {
		runner = TrustedRunner{}
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}

	clone, err := IsolatedClone(o.RepoDir, o.BaseSHA)
	if err != nil {
		return Report{}, err
	}
	if !o.KeepClone {
		defer os.RemoveAll(clone)
	}
	base, err := reader.ReadContract(clone)
	if err != nil {
		return Report{}, err
	}
	baseSurface, err := ReadSurface(clone)
	if err != nil {
		return Report{}, err
	}
	if err := detach(clone, o.CandidateSHA); err != nil {
		return Report{}, err
	}
	if err := copyWorkLedger(o.RepoDir, clone); err != nil {
		return Report{}, err
	}
	candidate, err := reader.ReadContract(clone)
	if err != nil {
		return Report{}, err
	}
	candSurface, err := ReadSurface(clone)
	if err != nil {
		return Report{}, err
	}

	dep, err := DependencyDigest(clone)
	if err != nil {
		return Report{}, err
	}
	changed := SpecChange(base, candidate)
	graded, capFrom, specFrom, refusal := gradingSpec(base, candidate, o.Spec, changed)
	review := ""
	if specFrom == SpecFromCandidate && o.Spec != VersionSpec {
		review = o.Spec
	}
	rec := evidence.Record{
		Binding: evidence.Binding{
			Candidate:  o.CandidateSHA,
			Base:       o.BaseSHA,
			Verifier:   BuildID(),
			Contract:   graded.Digest(),
			Dependency: dep,
			Principal:  o.Principal,
		},
		Verdict:    evidence.VerdictFail,
		Spec:       candidate.Digest(),
		SpecFrom:   specFrom,
		SpecChange: changed,
		SpecReview: review,
	}
	report := Report{Base: base, Candidate: candidate}
	if o.KeepClone {
		report.Clone = clone
	}

	finish := func(reason string, checks []evidence.CheckResult, uncalled []string) (Report, error) {
		rec.Checks = checks
		rec.Uncalled = uncalled
		rec.Reason = reason
		if reason == "" && len(uncalled) == 0 {
			rec.Verdict = evidence.VerdictPass
		}
		p, werr := evidence.Write(o.Home, rec)
		if werr != nil {
			return Report{}, werr
		}
		report.Record = rec
		report.Path = p
		return report, nil
	}

	// A spec change nobody authorized is refused before anything of the
	// candidate's runs: its checks would not be the ones it is graded by.
	if refusal != "" {
		return finish(refusal, nil, nil)
	}
	// The cap is the base's (a recorded version's own, on restore). A
	// candidate may only ever declare effects the base already allowed;
	// asking for more is an escalation and is refused before anything of
	// the candidate's runs.
	if ex := escalated(capFrom.Effects, candidate.Effects); len(ex) > 0 {
		return finish(fmt.Sprintf("effect cap escalation: the candidate declares %s, outside the base cap %s",
			strings.Join(ex, ","), capText(capFrom.Effects)), nil, nil)
	}
	capSpec := capFrom.Cap()
	if capSpec == "" {
		// contain refuses an empty cap, and so do we: an uncapped run is
		// not a contained one.
		return finish("the base contract declares no effect cap; an uncapped run proves nothing", nil, nil)
	}
	if _, err := advice.ParseCap(capSpec); err != nil {
		return finish(fmt.Sprintf("the base contract's effect cap is unusable: %v", err), nil, nil)
	}

	frozen, err := freezeChecks(clone, graded, candidate)
	if err != nil {
		return Report{}, err
	}
	if len(frozen) == 0 {
		return finish("no required checks: an unchecked candidate is unverified, not verified", nil, nil)
	}

	callLog := filepath.Join(clone, filepath.FromSlash(callLogRel))
	observed := map[string]bool{}
	var checks []evidence.CheckResult
	allPass := true
	for _, f := range frozen {
		// A fresh log per check so one check's calls are attributable and
		// a stale file cannot stand in for a run that never happened.
		if err := os.Remove(callLog); err != nil && !os.IsNotExist(err) {
			return Report{}, err
		}
		out, rerr := runner.RunCheck(ctx, CheckRun{
			Name: f.Name, Dir: clone, Script: filepath.Join(clone, filepath.FromSlash(StagedChecks), f.File),
			Cap: capSpec, CallLog: callLog, Timeout: timeout,
		})
		if rerr != nil {
			out = Outcome{Status: evidence.StatusFail, Detail: rerr.Error()}
		}
		calls := out.Calls
		if calls == nil {
			logged, lerr := readCallLog(callLog)
			if lerr != nil {
				return Report{}, lerr
			}
			calls = logged
		}
		for _, c := range calls {
			observed[c] = true
		}
		if out.Status != evidence.StatusPass {
			allPass = false
		}
		checks = append(checks, evidence.CheckResult{
			Name: f.Name, Status: out.Status, Detail: trim(out.Detail),
		})
	}

	compat := CompareSurface(baseSurface, candSurface, o.Contract)
	checks = append(checks, compat.Result())
	if compat.Failed() {
		allPass = false
	}

	seen := make([]string, 0, len(observed))
	for c := range observed {
		seen = append(seen, c)
	}
	sort.Strings(seen)
	// The accounting set is base AND candidate contracted functions. Base,
	// because a candidate that stops exercising the contract it inherited
	// has not been verified against it. Candidate, because contracted code
	// a candidate ADDS without a check that reaches it is unverified code —
	// and candidate checks do run, so adding one is always available.
	uncalled := missingCalls(union(graded.FunctionNames(), candidate.FunctionNames()), seen)

	reason := ""
	if !allPass {
		reason = "not every required check passed"
	}
	if compat.Failed() {
		reason = CompatCheck + ": " + compat.Detail()
	}
	return finish(reason, checks, uncalled)
}

// frozenCheck is one check as staged for execution: its contract name, the
// flat filename it was written to, and which commit its bytes came from.
type frozenCheck struct {
	Name string
	File string
	From string // "base" or "candidate"
}

// freezeChecks writes the check set into the verifier-owned staging
// directory inside the clone. Base checks are written with the BASE bytes,
// so a candidate's edit to a check has no effect; a check that exists only
// in the candidate is written too, so adding a check always counts.
func freezeChecks(clone string, base, candidate Contract) ([]frozenCheck, error) {
	dir := filepath.Join(clone, filepath.FromSlash(StagedChecks))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fromBase := map[string]bool{}
	var out []frozenCheck
	write := func(c Check, from string) error {
		sum := sha256.Sum256([]byte(c.Name))
		file := hex.EncodeToString(sum[:8]) + filepath.Ext(c.Name)
		if err := os.WriteFile(filepath.Join(dir, file), []byte(c.Body), 0o755); err != nil {
			return err
		}
		out = append(out, frozenCheck{Name: c.Name, File: file, From: from})
		return nil
	}
	for _, c := range base.Checks {
		fromBase[c.Name] = true
		if err := write(c, "base"); err != nil {
			return nil, err
		}
	}
	for _, c := range candidate.Checks {
		if fromBase[c.Name] {
			continue
		}
		if err := write(c, "candidate"); err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// union merges two sorted name lists, deduplicated and sorted.
func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string{}, a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// escalated returns the candidate effect atoms the base cap does not
// already allow, sorted.
func escalated(base, candidate []string) []string {
	var out []string
	for _, c := range candidate {
		if !slices.Contains(base, c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func capText(effects []string) string {
	if len(effects) == 0 {
		return "(none)"
	}
	return strings.Join(effects, ",")
}

// workLedger is the shaping ledger a candidate's work clone carries inside
// its .git (shape.WorkLedgerPath). It is never committed, so a clone does not
// copy it.
const workLedger = ".git/shape-ledger.json"

// copyWorkLedger hands the ledger from the repository being verified to the
// isolated clone, so a frozen check (a build or test step) reads the same
// file the agent saw. A repository without one is not an error.
func copyWorkLedger(repo, clone string) error {
	b, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(workLedger)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(clone, filepath.FromSlash(workLedger)), b, 0o600)
}

// IsolatedClone clones repo into a fresh temporary directory, detaches it
// at rev and strips the origin remote. The clone is isolated in both
// directions: nothing it does reaches the source repository, and nothing
// in it can follow a remote back to one.
func IsolatedClone(repo, rev string) (string, error) {
	if err := RequireCompleteSource(repo); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "shape-verify-")
	if err != nil {
		return "", err
	}
	if _, err := ygit.Clone(ygit.CloneOptions{URL: repo, Path: dir}); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("verify: clone %s: %w", repo, err)
	}
	// An escaped check must not be able to follow a remote back to the
	// repository it was cloned from.
	if _, err := ygit.RemoteRemove(dir, "origin"); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("verify: strip origin: %w", err)
	}
	if err := detach(dir, rev); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// detach moves an isolated clone to rev with a detached HEAD.
func detach(dir, rev string) error {
	if _, err := ygit.Checkout(ygit.CheckoutOptions{RepoPath: dir, Branch: rev}); err != nil {
		return fmt.Errorf("verify: detach at %s: %w", rev, err)
	}
	return nil
}

// dependencyManifests are the files whose contents decide what a build
// pulls in. A candidate that passes its checks with one dependency set has
// not been verified against another.
var dependencyManifests = map[string]bool{
	"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true,
	"package.json": true, "package-lock.json": true, "pnpm-lock.yaml": true,
	"yarn.lock": true, "requirements.txt": true, "pyproject.toml": true,
	"uv.lock": true, "Cargo.toml": true, "Cargo.lock": true,
}

// DependencyDigest fingerprints every dependency manifest in the tree. A
// tree with no manifests still has a digest — the digest of nothing — so
// "no dependencies" is a stated fact rather than an absent one.
func DependencyDigest(dir string) (string, error) {
	type row struct{ path, sum string }
	var rows []row
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !dependencyManifests[d.Name()] {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(data)
		rows = append(rows, row{filepath.ToSlash(rel), hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("verify: dependency digest: %w", err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	var b strings.Builder
	b.WriteString("shape-deps-v1\n")
	for _, r := range rows {
		b.WriteString(r.path + "\t" + r.sum + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// TrustedRunner dispatches each frozen check to the host's trusted
// interpreter inside the candidate clone, under pkg/shape/contain's bashy
// guards. The interpreter is installed on the host, not supplied by the
// candidate. No sandbox is required: the verifier's protection is that the
// checks and evidence are frozen outside the candidate's reach.
type TrustedRunner struct {
	// Bin is the interpreter; $BASHY_SHAPE_RUNNER, else "bashy".
	Bin string
	// Args renders the argv after Bin for one check script.
	Args func(script string) []string
	// Effects declares the interpreter's own effect atoms to contain,
	// which has no atlas entry for a host-installed interpreter. The
	// contract's cap is what actually bounds the checks, inside the
	// interpreter and through contain's cap; this only lets the run start.
	Effects []string
	// Level is the containment level; contain.LevelProcess by default.
	// contain.LevelSandbox is an explicit request routed to bashy oci.
	Level contain.Level
	// OCI routes a LevelSandbox run; nil = contain.BashyOCI{}.
	OCI contain.OCIRunner
	// EnvAllow lists parent variables the check may see. Nothing else is
	// inherited.
	EnvAllow []string
	// MaxRSSBytes and MaxProcs bound one check's resource use.
	MaxRSSBytes uint64
	MaxProcs    int
}

// RunCheck runs one frozen check and classifies its exit.
func (r TrustedRunner) RunCheck(ctx context.Context, c CheckRun) (Outcome, error) {
	bin := r.Bin
	if bin == "" {
		bin = strings.TrimSpace(os.Getenv("BASHY_SHAPE_RUNNER"))
	}
	bin = selfbin.Resolve(bin)
	argv := []string{bin, c.Script}
	if r.Args != nil {
		argv = append([]string{bin}, r.Args(c.Script)...)
	}
	effects := r.Effects
	if len(effects) == 0 {
		// The interpreter can do whatever the contract permits; saying so
		// explicitly keeps contain's cap check meaningful for the atoms
		// that matter instead of denying the interpreter as unknown.
		effects = strings.Split(c.Cap, ",")
	}
	level := r.Level
	if level == "" {
		level = contain.LevelProcess
	}
	out, err := os.CreateTemp(c.Dir, ".check-out-")
	if err != nil {
		return Outcome{}, err
	}
	defer os.Remove(out.Name())
	defer out.Close()

	res, err := contain.Run(ctx, contain.Spec{
		Dir: c.Dir, Argv: argv, Level: level, OCI: r.OCI, Cap: c.Cap, Effects: effects,
		EnvAllow: r.EnvAllow,
		EnvSet:   checkEnv(c.CallLog),
		Timeout:  c.Timeout, MaxRSSBytes: r.MaxRSSBytes, MaxProcs: r.MaxProcs,
		Stdout: out, Stderr: out,
	})
	detail := ""
	if b, rerr := os.ReadFile(out.Name()); rerr == nil {
		detail = string(b)
	}
	if err != nil {
		// A refusal is not a failing check: it is a check that did not
		// run, and an unrun check never passes a verdict.
		return Outcome{Status: evidence.StatusSkip, Detail: err.Error()}, nil
	}
	switch {
	case ctx.Err() != nil:
		return Outcome{Status: evidence.StatusCancel, Detail: detail}, nil
	case res.Limited == "timeout":
		return Outcome{Status: evidence.StatusTimeout, Detail: detail}, nil
	case res.Limited != "":
		return Outcome{Status: evidence.StatusFail, Detail: res.Limited + " limit: " + detail}, nil
	case res.ExitCode == 0:
		return Outcome{Status: evidence.StatusPass, Detail: detail}, nil
	case res.ExitCode == SkipExit:
		return Outcome{Status: evidence.StatusSkip, Detail: detail}, nil
	}
	return Outcome{Status: evidence.StatusFail, Detail: detail}, nil
}

// checkEnv is what a check sees on top of the scrubbed floor: its call log
// and, because a check builds the candidate inside the clone, the host's
// local module proxy for shared modules, read from the verifier's own
// environment before the isolated home replaces it.
func checkEnv(callLog string) map[string]string {
	env := map[string]string{CallLogEnv: callLog}
	for _, kv := range gomod.BuildEnv(gomod.ProxyDir(), nil) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	// A check runs with HOME inside the clone, so Go would default its module
	// and build caches there and download the toolchain and every dependency
	// again for each check: eight parallel forks blow the per-check budget.
	// Share the host's read-only caches instead, as the warm-cache rule asks;
	// the verifier's own values win, else what the go tool reports.
	for k, v := range goCaches() {
		env[k] = v
	}
	// Resolve managed tools before HOME is isolated, so checks using bashy go
	// reuse the host's toolchain instead of downloading it into each clone.
	if cache, err := binmgr.CacheDir(); err == nil {
		env["BASHY_BIN_CACHE"] = cache
	}
	return env
}

// goCaches names the host's Go module and build caches: the verifier's
// environment first, else `go env`; absent when neither answers.
func goCaches() map[string]string {
	out := map[string]string{}
	missing := []string{}
	for _, k := range []string{"GOMODCACHE", "GOCACHE"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			out[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		if res, err := exec.Command("go", append([]string{"env"}, missing...)...).Output(); err == nil {
			for i, line := range strings.Split(strings.TrimSpace(string(res)), "\n") {
				if i < len(missing) && strings.TrimSpace(line) != "" {
					out[missing[i]] = strings.TrimSpace(line)
				}
			}
		}
	}
	return out
}

// trim bounds a detail string stored in a record.
func trim(s string) string {
	const max = 2000
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "\n…(truncated)"
	}
	return s
}

// Binding recomputes the parts of an evidence binding that a reader can
// derive from the repository itself — the frozen contract digest and the
// dependency digest at the candidate — plus the running verifier's build
// id. A gate calls it so it compares a record against inputs it worked out
// for itself rather than against the record's own account of them. The
// contract digest is the base's, or the candidate's own when the candidate
// changes the spec: a spec change is only ever graded against its own spec.
func Binding(repoDir, candidateSHA, baseSHA, principal string, reader ContractReader) (evidence.Binding, error) {
	b, _, err := BindingSpec(repoDir, candidateSHA, baseSHA, principal, reader)
	return b, err
}

// BindingSpec is Binding plus the spec delta it derived, so a gate can also
// demand that a spec change was graded under the owner's review of exactly
// that spec (RequireSpec).
func BindingSpec(repoDir, candidateSHA, baseSHA, principal string, reader ContractReader) (evidence.Binding, SpecDelta, error) {
	if repoDir == "" || candidateSHA == "" || baseSHA == "" {
		return evidence.Binding{}, SpecDelta{}, errors.New("verify: repo dir, base and candidate are required")
	}
	if reader == nil {
		reader = UnitReader{}
	}
	clone, err := IsolatedClone(repoDir, baseSHA)
	if err != nil {
		return evidence.Binding{}, SpecDelta{}, err
	}
	defer os.RemoveAll(clone)
	base, err := reader.ReadContract(clone)
	if err != nil {
		return evidence.Binding{}, SpecDelta{}, err
	}
	if err := detach(clone, candidateSHA); err != nil {
		return evidence.Binding{}, SpecDelta{}, err
	}
	cand, err := reader.ReadContract(clone)
	if err != nil {
		return evidence.Binding{}, SpecDelta{}, err
	}
	dep, err := DependencyDigest(clone)
	if err != nil {
		return evidence.Binding{}, SpecDelta{}, err
	}
	delta := SpecDelta{Base: base.Digest(), Candidate: cand.Digest(), Changed: SpecChange(base, cand)}
	graded := delta.Base
	if len(delta.Changed) > 0 {
		graded = delta.Candidate
	}
	return evidence.Binding{
		Candidate:  candidateSHA,
		Base:       baseSHA,
		Verifier:   BuildID(),
		Contract:   graded,
		Dependency: dep,
		Principal:  principal,
	}, delta, nil
}
