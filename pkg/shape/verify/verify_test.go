package verify

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/shape/evidence"
)

const unit = `@guard(effects: "read,write")
@require('true')
@ensure('test -f out.txt')
build() {
	: > out.txt
}

# not contracted: no decorator above it
helper() {
	true
}
`

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDecoratorReader(t *testing.T) {
	dir := tree(t, map[string]string{
		"app.bsh":          unit,
		"checks/smoke.bsh": "run build\n",
		"docs/SKILL.md":    "prose\n\n```bsh\n@ensure('true')\nping() { true; }\n```\n",
		"README.md":        "no fences here\n",
	})
	c, err := DecoratorReader{}.ReadContract(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.FunctionNames(); strings.Join(got, ",") != "build,ping" {
		t.Fatalf("functions = %v, want build and ping (helper carries no decorator)", got)
	}
	if len(c.Checks) != 1 || c.Checks[0].Name != "checks/smoke.bsh" {
		t.Fatalf("checks = %+v, want checks/smoke.bsh", c.Checks)
	}
	if c.Cap() != "read,write" {
		t.Fatalf("cap = %q, want read,write", c.Cap())
	}
}

// The digest is what an evidence binding pins the contract with: equal
// contracts must digest equally, and every part of a contract must move it.
func TestContractDigestIsStableAndSensitive(t *testing.T) {
	base := map[string]string{"app.bsh": unit, "checks/smoke.bsh": "run build\n"}
	want, err := DecoratorReader{}.ReadContract(tree(t, base))
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecoratorReader{}.ReadContract(tree(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if want.Digest() != again.Digest() {
		t.Fatal("the same contract digested differently twice")
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"decorator", map[string]string{
			"app.bsh":          strings.Replace(unit, `@ensure('test -f out.txt')`, `@ensure('true')`, 1),
			"checks/smoke.bsh": "run build\n"}},
		{"check body", map[string]string{"app.bsh": unit, "checks/smoke.bsh": "exit 0\n"}},
		{"effects", map[string]string{
			"app.bsh":          strings.Replace(unit, `"read,write"`, `"read"`, 1),
			"checks/smoke.bsh": "run build\n"}},
		{"new function", map[string]string{
			"app.bsh":          unit + "\n@ensure('true')\nextra() { true; }\n",
			"checks/smoke.bsh": "run build\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecoratorReader{}.ReadContract(tree(t, tc.files))
			if err != nil {
				t.Fatal(err)
			}
			if got.Digest() == want.Digest() {
				t.Fatalf("%s did not move the digest", tc.name)
			}
		})
	}
	// A body change that leaves the contract alone leaves the digest alone:
	// the digest pins the contract, not the implementation.
	same, err := DecoratorReader{}.ReadContract(tree(t, map[string]string{
		"app.bsh":          strings.Replace(unit, "\t: > out.txt", "\tprintf '' > out.txt", 1),
		"checks/smoke.bsh": "run build\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if same.Digest() != want.Digest() {
		t.Fatal("an implementation change moved the contract digest")
	}
}

func TestDependencyDigestTracksManifests(t *testing.T) {
	a, err := DependencyDigest(tree(t, map[string]string{"go.mod": "module a\n"}))
	if err != nil {
		t.Fatal(err)
	}
	b, err := DependencyDigest(tree(t, map[string]string{"go.mod": "module a\n\nrequire x v1\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("a changed go.mod did not move the dependency digest")
	}
	empty, err := DependencyDigest(tree(t, map[string]string{"app.bsh": unit}))
	if err != nil {
		t.Fatal(err)
	}
	if empty == "" || empty == a {
		t.Fatalf("a tree with no manifests needs its own digest, got %q", empty)
	}
}

// Base checks are frozen at the base's bytes; a candidate's edit to one is
// ignored, and a check the candidate adds is staged as well.
func TestFreezeChecksPrefersTheBaseBytes(t *testing.T) {
	clone := t.TempDir()
	base := Contract{Checks: []Check{{Name: "checks/smoke.bsh", Body: "base\n"}}}
	candidate := Contract{Checks: []Check{
		{Name: "checks/smoke.bsh", Body: "neutered\n"},
		{Name: "checks/extra.bsh", Body: "added\n"},
	}}
	frozen, err := freezeChecks(clone, base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 2 {
		t.Fatalf("frozen = %+v, want both checks", frozen)
	}
	for _, f := range frozen {
		raw, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(StagedChecks), f.File))
		if err != nil {
			t.Fatal(err)
		}
		switch f.Name {
		case "checks/smoke.bsh":
			if string(raw) != "base\n" || f.From != "base" {
				t.Fatalf("smoke staged %q from %q, want the base bytes", raw, f.From)
			}
		case "checks/extra.bsh":
			if string(raw) != "added\n" || f.From != "candidate" {
				t.Fatalf("extra staged %q from %q, want the candidate's", raw, f.From)
			}
		}
	}
}

func TestRecordCallIsANoOpWithoutALog(t *testing.T) {
	t.Setenv(CallLogEnv, "")
	RecordCall("build") // must not panic or write anywhere

	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(CallLogEnv, log)
	RecordCall("build")
	RecordCall("build")
	RecordCall("report")
	got, err := readCallLog(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "build,report" {
		t.Fatalf("calls = %v, want build and report once each", got)
	}
	if missing := missingCalls([]string{"build", "report", "audit"}, got); strings.Join(missing, ",") != "audit" {
		t.Fatalf("missing = %v, want audit", missing)
	}
}

// Every exit a check can make maps to exactly one status, and only pass
// is a pass. A skip is an unproven check, not a lenient one.
func TestTrustedRunnerClassifiesEveryExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture checks are POSIX shell")
	}
	for _, tc := range []struct {
		name, script string
		timeout      time.Duration
		want         string
	}{
		{"pass", "exit 0\n", time.Minute, evidence.StatusPass},
		{"fail", "exit 3\n", time.Minute, evidence.StatusFail},
		{"skip", "exit 77\n", time.Minute, evidence.StatusSkip},
		{"timeout", "sleep 30\n", 300 * time.Millisecond, evidence.StatusTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "check.sh")
			if err := os.WriteFile(script, []byte(tc.script), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := TrustedRunner{
				Bin: "sh", Effects: []string{"read"}, Level: contain.LevelProcess,
			}.RunCheck(context.Background(), CheckRun{
				Name: tc.name, Dir: dir, Script: script, Cap: "read",
				CallLog: filepath.Join(dir, "calls.log"), Timeout: tc.timeout,
			})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != tc.want {
				t.Fatalf("status = %q, want %q (%s)", out.Status, tc.want, out.Detail)
			}
		})
	}
}

// A run the host cannot contain is a check that did not happen, and an
// unrun check never passes a verdict.
func TestARefusedRunIsNotAPass(t *testing.T) {
	dir := t.TempDir()
	out, err := TrustedRunner{
		Bin: "sh", Effects: []string{"net"}, Level: contain.LevelProcess,
	}.RunCheck(context.Background(), CheckRun{
		Name: "capped", Dir: dir, Script: filepath.Join(dir, "check.sh"), Cap: "read",
		CallLog: filepath.Join(dir, "calls.log"), Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status == evidence.StatusPass {
		t.Fatalf("a refused run reported %q", out.Status)
	}
}

func TestRunNeedsAPrincipal(t *testing.T) {
	_, err := Run(context.Background(), Options{
		Home: t.TempDir(), RepoDir: t.TempDir(), BaseSHA: "a", CandidateSHA: "b",
	})
	if err == nil || !strings.Contains(err.Error(), "principal") {
		t.Fatalf("err = %v, want a principal refusal", err)
	}
}
