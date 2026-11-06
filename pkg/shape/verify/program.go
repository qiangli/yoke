package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/shape/evidence"
)

// ProgramOptions is trusted registry policy, supplied by the host, not source.
// Existing checker and trust-root paths have already been compared with BaseSHA.
type ProgramOptions struct {
	RetainedArtifact                                                   string
	CheckArtifact                                                      func(context.Context, string, string) error
	Home, App, Kind, RepoDir, BaseSHA, CandidateSHA, Principal, Policy string
	Build, Test, GuardedPaths                                          []string
	Runner                                                             ProgramRunner
}
type ProgramRun struct {
	Output   *string
	Artifact string
	Dir      string
	Argv     []string
	Timeout  time.Duration
}
type ProgramRunner interface {
	RunProgram(context.Context, ProgramRun) error
}
type ProgramRunnerFunc func(context.Context, ProgramRun) error

func (f ProgramRunnerFunc) RunProgram(ctx context.Context, r ProgramRun) error { return f(ctx, r) }

type ContainedProgramRunner struct{ runtimeRoot string }

type programCapture struct {
	bytes.Buffer
	overflow bool
	limit    int
}

func (b *programCapture) Write(p []byte) (int, error) {
	n := len(p)
	left := b.limit - b.Len()
	if left < len(p) {
		b.overflow = true
		if left > 0 {
			_, _ = b.Buffer.Write(p[:left])
		}
	} else {
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
func (runner ContainedProgramRunner) RunProgram(ctx context.Context, r ProgramRun) error {
	if len(r.Argv) == 0 {
		return errors.New("empty program argv")
	}
	root := runner.runtimeRoot
	if root == "" {
		var e error
		root, e = newProgramRuntimeRoot(r.Dir)
		if e != nil {
			return e
		}
		defer removeProgramRuntimeRoot(root)
	}
	source, e := filepath.EvalSymlinks(r.Dir)
	if e != nil {
		return e
	}
	if programPathInside(source, root) {
		return errors.New("program runtime home/temp must be outside source")
	}
	env, e := programRuntimeEnvironment(root, runtime.GOOS)
	if e != nil {
		return e
	}
	outR, outW, e := os.Pipe()
	if e != nil {
		return e
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, e := os.Pipe()
	if e != nil {
		return e
	}
	defer errR.Close()
	defer errW.Close()
	stdout := programCapture{limit: 32 << 20}
	stderr := programCapture{limit: 1 << 20}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); _, _ = io.Copy(&stdout, outR) }()
	go func() { defer readers.Done(); _, _ = io.Copy(&stderr, errR) }()
	selected, e := exec.LookPath(r.Argv[0])
	if e == nil {
		selected, _ = filepath.Abs(selected)
	} else {
		selected = r.Argv[0]
	}
	env["GOWORK"] = "off"
	env["BASHY_SHAPE_ENTRY"] = selected
	if filepath.IsAbs(r.Artifact) {
		env["BASHY_BIN"] = r.Artifact
	}
	result, runErr := contain.Run(ctx, contain.Spec{Dir: r.Dir, Argv: r.Argv, Cap: "read,write,exec,net", Effects: []string{"read", "write", "exec", "net"}, EnvAllow: []string{"PATH"}, EnvSet: env, Timeout: r.Timeout, MaxProcs: 128, MaxRSSBytes: 8 << 30, Stdout: outW, Stderr: errW})
	_ = outW.Close()
	_ = errW.Close()
	done := make(chan struct{})
	go func() { readers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = outR.Close()
		_ = errR.Close()
		<-done
		stdout.overflow = true
	}
	if r.Output != nil {
		*r.Output = stdout.String()
	}
	if runErr != nil {
		return runErr
	}
	if stdout.overflow || stderr.overflow {
		return errors.New("program check output limit exceeded")
	}
	if result.ExitCode != 0 || result.Limited != "" {
		detail := stderr.String()
		if detail == "" {
			detail = stdout.String()
		}
		if len(detail) > 4096 {
			detail = detail[len(detail)-4096:]
		}
		return fmt.Errorf("program check exited %d (%s): %s", result.ExitCode, result.Limited, strings.TrimSpace(detail))
	}
	return nil
}

type ProgramReport struct {
	Record                       evidence.Record
	Path, ArtifactPath, Artifact string
}

func expandProgram(argv []string, output, artifact, revision string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		a = strings.ReplaceAll(a, "{output}", output)
		a = strings.ReplaceAll(a, "{artifact}", artifact)
		a = strings.ReplaceAll(a, "{revision}", revision)
		out[i] = a
	}
	return out
}
func fileDigest(path string) (string, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("program output must be a regular file, not a symlink")
	}
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil {
		return "", e
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("program output is not a regular file")
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
func imageDigest(path string) (string, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("image receipt must be a regular file, not a symlink")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return "", e
	}
	if len(b) > 4096 {
		return "", errors.New("image receipt too large")
	}
	v := strings.TrimSpace(string(b))
	raw := strings.TrimPrefix(v, "sha256:")
	decoded, e := hex.DecodeString(raw)
	if e != nil || len(decoded) != sha256.Size || v != "sha256:"+strings.ToLower(raw) {
		return "", errors.New("build must write an immutable sha256 image ID")
	}
	return v, nil
}
func guardProgramTree(dir string, guards []string) error {
	r, e := gogit.PlainOpen(dir)
	if e != nil {
		return e
	}
	w, e := r.Worktree()
	if e != nil {
		return e
	}
	st, e := w.Status()
	if e != nil {
		return e
	}
	for path, status := range st {
		if status.Staging != gogit.Untracked || status.Worktree != gogit.Untracked {
			return fmt.Errorf("build/test changed tracked source %s", path)
		}
		for _, g := range guards {
			if path == g || strings.HasPrefix(path, g+"/") {
				return fmt.Errorf("build/test created protected source %s", path)
			}
		}
	}
	return nil
}

// RunProgram runs both required argv gates on an isolated exact source tree.
// The trusted caller computes Policy from frozen registry/config and guard paths.
func RunProgram(ctx context.Context, o ProgramOptions) (ProgramReport, error) {
	if o.Home == "" || o.App == "" || filepath.Base(o.App) != o.App || strings.ContainsAny(o.App, "/\\") || o.BaseSHA == "" || o.CandidateSHA == "" || o.Principal == "" || o.Policy == "" || len(o.Build) == 0 || len(o.Test) == 0 {
		return ProgramReport{}, errors.New("program verifier needs complete trusted policy and identities")
	}
	if o.Kind != "cli" && o.Kind != "service" {
		return ProgramReport{}, errors.New("program kind must be cli or service")
	}
	runner := o.Runner
	if runner == nil {
		if e := RequireCompleteSource(o.RepoDir); e != nil {
			return ProgramReport{}, e
		}
		root, e := newProgramRuntimeRoot(o.RepoDir)
		if e != nil {
			return ProgramReport{}, e
		}
		defer removeProgramRuntimeRoot(root)
		runner = ContainedProgramRunner{runtimeRoot: root}
	}
	clone, e := IsolatedClone(o.RepoDir, o.CandidateSHA)
	if e != nil {
		return ProgramReport{}, e
	}
	defer os.RemoveAll(clone)
	dependency, e := DependencyDigest(clone)
	if e != nil {
		return ProgramReport{}, e
	}
	rec := evidence.Record{Binding: evidence.Binding{Candidate: o.CandidateSHA, Base: o.BaseSHA, Verifier: BuildID(), Contract: o.Policy, Policy: o.Policy, Dependency: dependency, Principal: o.Principal}, Verdict: evidence.VerdictFail}
	report := ProgramReport{}
	finish := func(reason string) (ProgramReport, error) {
		rec.Reason = reason
		if reason == "" {
			rec.Verdict = evidence.VerdictPass
		}
		path, e := evidence.Write(o.Home, rec)
		report.Record = rec
		report.Path = path
		return report, e
	}
	scratch, e := os.MkdirTemp(clone, ".shape-program-")
	if e != nil {
		return report, e
	}
	output := filepath.Join(scratch, o.App)
	if o.Kind == "cli" && runtime.GOOS == "windows" {
		output += ".exe"
	}
	expected, native, e := programBaselineChecks(ctx, o, runner, clone)
	if e != nil {
		return finish(e.Error())
	}
	build := expandProgram(o.Build, output, "", o.CandidateSHA)
	if e = runner.RunProgram(ctx, ProgramRun{Dir: clone, Argv: build, Timeout: 30 * time.Minute}); e != nil {
		rec.Checks = append(rec.Checks, evidence.CheckResult{Name: "build", Status: evidence.StatusFail, Detail: e.Error()})
		return finish("build failed")
	}
	rec.Checks = append(rec.Checks, evidence.CheckResult{Name: "build", Status: evidence.StatusPass})
	artifactFile := output
	if o.RetainedArtifact != "" {
		artifactFile = o.RetainedArtifact
	}
	artifact := artifactFile
	if o.Kind == "service" {
		artifact, e = imageDigest(artifactFile)
		if e != nil {
			return finish(e.Error())
		}
	}
	test := expandProgram(o.Test, output, artifact, o.CandidateSHA)
	var testOutput string
	if e = runner.RunProgram(ctx, ProgramRun{Dir: clone, Argv: test, Artifact: artifact, Output: &testOutput, Timeout: 30 * time.Minute}); e != nil {
		rec.Checks = append(rec.Checks, evidence.CheckResult{Name: "test", Status: evidence.StatusFail, Detail: e.Error()})
		return finish("declared tests failed")
	}
	rec.Checks = append(rec.Checks, evidence.CheckResult{Name: "test", Status: evidence.StatusPass})
	if native {
		results, e := accountProgramGoTests(testOutput, expected)
		rec.Checks = append(rec.Checks, results...)
		if e != nil {
			return finish(e.Error())
		}
	}
	if e = guardProgramTree(clone, o.GuardedPaths); e != nil {
		return finish(e.Error())
	}
	after, e := DependencyDigest(clone)
	if e != nil {
		return report, e
	}
	if after != dependency {
		return finish("build/test changed dependency manifests")
	}
	if o.Kind == "service" {
		after, e := imageDigest(artifactFile)
		if e != nil || after != artifact {
			return finish("image receipt changed during tests")
		}
		report.Artifact = artifact
	} else {
		report.Artifact, e = fileDigest(artifact)
		if e != nil {
			return finish(e.Error())
		}
	}
	rec.Binding.Artifact = report.Artifact
	dest := filepath.Join(o.Home, "src", ".shape-artifacts", o.App, strings.TrimPrefix(report.Artifact, "sha256:"))
	if e = os.MkdirAll(dest, 0700); e != nil {
		return report, e
	}
	name := o.App
	if o.Kind == "cli" && runtime.GOOS == "windows" {
		name += ".exe"
	}
	report.ArtifactPath = filepath.Join(dest, name)
	in, e := os.Open(artifactFile)
	if e != nil {
		return report, e
	}
	defer in.Close()
	tmp, e := os.CreateTemp(dest, ".artifact-*")
	if e != nil {
		return report, e
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if e = tmp.Chmod(0700); e == nil {
		_, e = io.Copy(tmp, in)
	}
	if e == nil {
		e = tmp.Sync()
	}
	ce := tmp.Close()
	if e != nil {
		return report, e
	}
	if ce != nil {
		return report, ce
	}
	copied, e := ProgramArtifactDigest(o.Kind, tmpName)
	if e != nil {
		return report, e
	}
	if copied != report.Artifact {
		return finish("artifact changed while staging")
	}
	if e = os.Rename(tmpName, report.ArtifactPath); e != nil {
		return report, e
	}
	if runtime.GOOS != "windows" {
		dir, e := os.Open(dest)
		if e != nil {
			return report, e
		}
		e = dir.Sync()
		_ = dir.Close()
		if e != nil {
			return report, e
		}
	}

	if o.CheckArtifact != nil {
		if e = o.CheckArtifact(ctx, report.Artifact, report.ArtifactPath); e != nil {
			rec.Checks = append(rec.Checks, evidence.CheckResult{Name: "service-health", Status: evidence.StatusFail, Detail: e.Error()})
			return finish("disposable service health failed")
		}
		rec.Checks = append(rec.Checks, evidence.CheckResult{Name: "service-health", Status: evidence.StatusPass})
	}
	return finish("")
}

// ProgramArtifactDigest rechecks the bytes/receipt staged by the trusted verifier.
func ProgramArtifactDigest(kind, path string) (string, error) {
	if kind == "service" {
		return imageDigest(path)
	}
	return fileDigest(path)
}

// IsDependencyManifest reports the manifest names fingerprinted by DependencyDigest.
func IsDependencyManifest(name string) bool { return dependencyManifests[name] }
