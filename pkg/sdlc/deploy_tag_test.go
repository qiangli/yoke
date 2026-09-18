package sdlc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployTagScheme(t *testing.T) {
	cases := map[[2]string]string{
		{"v0.0.1", "dev"}:  "v0.0.1-dev",
		{"v0.0.1", "qa"}:   "v0.0.1-qa",
		{"v0.0.1", "prod"}: "v0.0.1", // prod = bare version
		{"v0.0.1", ""}:     "v0.0.1",
		{"v0.0.1", "PROD"}: "v0.0.1",
		{"v1.2.3", "qa"}:   "v1.2.3-qa",
	}
	for in, want := range cases {
		if got := deployTag(in[0], in[1]); got != want {
			t.Errorf("deployTag(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestDeployOnceIsIdempotent proves the core guarantee: once v0.0.1-qa is
// deployed, re-firing the deploy (a flipped/re-applied deploy:qa label) is a
// no-op — the command runs exactly once. A different version deploys again.
func TestDeployOnceIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	bare := t.TempDir()
	git(t, "", "init", "--bare", bare)
	work := t.TempDir()
	git(t, "", "clone", bare, work)
	git(t, work, "commit", "--allow-empty", "-m", "init")
	git(t, work, "push", "origin", "HEAD:refs/heads/main")

	counter := filepath.Join(work, "deploys.log")
	// ToSlash: on Windows the native path has backslashes, which `sh -c` treats as
	// escapes (writing to the wrong file → the test saw 0 runs). Git Bash accepts
	// C:/… forward-slash paths. os.ReadFile below still uses the native `counter`.
	deploy := []string{"sh", "-c", "printf x >> " + filepath.ToSlash(counter)} // records each real deploy
	ctx := context.Background()

	// 1) first deploy of v0.0.1 to qa → runs, tags v0.0.1-qa
	r1, err := DeployOnce(ctx, DeployOnceOptions{Version: "v0.0.1", Env: "qa", Cwd: work, Command: deploy})
	if err != nil || r1.Status != "deployed" || r1.Tag != "v0.0.1-qa" {
		t.Fatalf("first deploy: status=%q tag=%q err=%v", r1.Status, r1.Tag, err)
	}
	// 2) re-fire the SAME version+env (label flipped back and forth) → no-op
	r2, err := DeployOnce(ctx, DeployOnceOptions{Version: "v0.0.1", Env: "qa", Cwd: work, Command: deploy})
	if err != nil || r2.Status != "already-deployed" {
		t.Fatalf("re-fire must be a no-op: status=%q err=%v", r2.Status, err)
	}
	r3, _ := DeployOnce(ctx, DeployOnceOptions{Version: "v0.0.1", Env: "qa", Cwd: work, Command: deploy})
	if r3.Status != "already-deployed" {
		t.Fatalf("third re-fire: status=%q, want already-deployed", r3.Status)
	}
	// the deploy command ran EXACTLY ONCE despite three fires
	data, _ := os.ReadFile(counter)
	if n := strings.Count(string(data), "x"); n != 1 {
		t.Fatalf("deploy command ran %d times across 3 fires, want 1 (idempotent)", n)
	}

	// 3) a NEW version deploys again (v0.0.2-qa isn't tagged yet)
	r4, err := DeployOnce(ctx, DeployOnceOptions{Version: "v0.0.2", Env: "qa", Cwd: work, Command: deploy})
	if err != nil || r4.Status != "deployed" || r4.Tag != "v0.0.2-qa" {
		t.Fatalf("new version: status=%q tag=%q err=%v", r4.Status, r4.Tag, err)
	}
	// prod uses the bare tag
	r5, _ := DeployOnce(ctx, DeployOnceOptions{Version: "v0.0.2", Env: "prod", Cwd: work, Command: deploy})
	if r5.Status != "deployed" || r5.Tag != "v0.0.2" {
		t.Fatalf("prod deploy: status=%q tag=%q, want deployed/v0.0.2", r5.Status, r5.Tag)
	}
}
