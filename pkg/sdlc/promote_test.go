package sdlc

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
)

func TestRequiresApproval(t *testing.T) {
	cfg := Config{Deploy: DeploymentConfig{Production: TargetConfig{Environment: "prod"}}}

	// prod + default policy (absent) → required
	if !RequiresApproval(cfg, "prod") {
		t.Fatal("prod should require approval by default")
	}
	// prod + explicit auto → not required
	cfg.Policies = map[string]string{"prod_approval": "auto"}
	if RequiresApproval(cfg, "prod") {
		t.Fatal("prod_approval=auto should not require approval")
	}
	// non-prod env → never required
	cfg.Policies = nil
	if RequiresApproval(cfg, "qa") {
		t.Fatal("qa should not require approval")
	}
	// matches by Name too
	cfg = Config{Deploy: DeploymentConfig{Production: TargetConfig{Name: "prod"}}}
	if !RequiresApproval(cfg, "prod") {
		t.Fatal("prod (matched by Name) should require approval")
	}
}

func TestResolvePromoteTarget(t *testing.T) {
	if repo, n, ok := resolvePromoteTarget(RunRecord{IssueID: "acme/app#12"}, ""); !ok || repo != "acme/app" || n != 12 {
		t.Fatalf("got (%q,%d,%v)", repo, n, ok)
	}
	// bare number + override
	if repo, n, ok := resolvePromoteTarget(RunRecord{IssueID: "7"}, "acme/app"); !ok || repo != "acme/app" || n != 7 {
		t.Fatalf("override case got (%q,%d,%v)", repo, n, ok)
	}
	// unresolvable
	if _, _, ok := resolvePromoteTarget(RunRecord{IssueID: "local-only"}, ""); ok {
		t.Fatal("expected unresolvable")
	}
}

// makeApprovedRun writes an approved run to a temp dir and returns (dir, id).
func makeApprovedRun(t *testing.T, issueID string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	res := DelegateResult{Conductor: "codex", Issue: Issue{ID: issueID, Title: "Ship it"}}
	run, err := NewRunRecord(res, DelegateOptions{RunsDir: dir, Cwd: dir}, chat.Result{Agent: "codex", Cwd: dir, Args: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "succeeded"
	run.FinishedAt = time.Now().UTC()
	if err := SaveRunRecord(*run); err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveRun(dir, run.ReferenceID, "ok", "user"); err != nil {
		t.Fatal(err)
	}
	return dir, run.ReferenceID
}

func TestPromoteCommand_AppliesLabel(t *testing.T) {
	reqs := captureGitHub(t, http.StatusOK)
	t.Setenv("GITHUB_TOKEN", "tok")
	dir, id := makeApprovedRun(t, "acme/app#12")

	cmd := NewSDLCCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"promote", id, "--env", "qa", "--runs-dir", dir, "--config", dir + "/missing.yaml", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"label":"deploy:qa"`) {
		t.Fatalf("unexpected output: %s", out.String())
	}
	if len(*reqs) != 1 || (*reqs)[0].path != "/repos/acme/app/issues/12/labels" {
		t.Fatalf("expected label POST, got %+v", *reqs)
	}
}

func TestPromoteCommand_ProdRequiresApproval(t *testing.T) {
	captureGitHub(t, http.StatusOK)
	t.Setenv("GITHUB_TOKEN", "tok")

	// Build an UN-approved run.
	dir := t.TempDir()
	res := DelegateResult{Conductor: "codex", Issue: Issue{ID: "acme/app#5", Title: "Ship"}}
	run, _ := NewRunRecord(res, DelegateOptions{RunsDir: dir, Cwd: dir}, chat.Result{Agent: "codex", Cwd: dir})
	run.Status = "succeeded"
	_ = SaveRunRecord(*run)

	// A config where prod env requires approval (default).
	cfgPath := dir + "/sdlc.yaml"
	writeFile(t, cfgPath, "conductor:\n  agent: codex\nintake:\n  provider: github\ndeployment:\n  staging: {name: qa, environment: qa}\n  production: {name: prod, environment: prod}\n")

	cmd := NewSDLCCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"promote", run.ReferenceID, "--env", "prod", "--runs-dir", dir, "--config", cfgPath})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("expected approval-required error, got %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
