package lenscmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens"
)

func runToolWithEnv(t *testing.T, dir, stdin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		Env:   env,
		Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader(stdin)},
	}
	code := cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestS14EnvThroughLens(t *testing.T) {
	dir := t.TempDir()
	fakeEnv := []string{
		"BETA=2",
		"ALPHA=1",
		"SECRET_KEY=super-secret-password-12345",
	}

	// 1. Paths view produces sorted deterministic rows
	out, errb, code := runToolWithEnv(t, dir, "", fakeEnv, "-t", "paths", "env:")
	if code != 0 || errb != "" {
		t.Fatalf("paths env: %d %q", code, errb)
	}
	wantPaths := "env:\nenv:ALPHA\nenv:BETA\nenv:SECRET_KEY\n"
	if out != wantPaths {
		t.Errorf("paths env:\n%s\nwant:\n%s", out, wantPaths)
	}

	// Verify secrets are never emitted in output or stderr
	for _, view := range lens.GraphViews {
		vOut, vErr, c := runToolWithEnv(t, dir, "", fakeEnv, "-t", view, "env:")
		if c != 0 {
			t.Errorf("view %s exited with %d: %s", view, c, vErr)
		}
		if strings.Contains(vOut, "super-secret-password-12345") || strings.Contains(vErr, "super-secret-password-12345") {
			t.Fatalf("SECRET LEAKED in view %s: out=%q err=%q", view, vOut, vErr)
		}
	}

	// 2. Specific variable
	outVar, errbVar, codeVar := runToolWithEnv(t, dir, "", fakeEnv, "-t", "paths", "env:ALPHA")
	if codeVar != 0 || errbVar != "" || outVar != "env:ALPHA\n" {
		t.Fatalf("paths env:ALPHA: %d %q out=%q", codeVar, errbVar, outVar)
	}

	// 3. Pull and push give identical bytes across all graph views
	produced, errb, code := runToolWithEnv(t, dir, "", fakeEnv, "-t", "graph", "env:")
	if code != 0 || errb != "" {
		t.Fatalf("produce graph: %d %q", code, errb)
	}
	if strings.Contains(produced, "super-secret-password-12345") {
		t.Fatalf("SECRET LEAKED in graph output: %s", produced)
	}

	for _, view := range lens.GraphViews {
		pulled, _, c1 := runToolWithEnv(t, dir, "", fakeEnv, "-t", view, "env:")
		pushed, _, c2 := runToolWithEnv(t, dir, produced, fakeEnv, "-t", view)
		if c1 != 0 || c2 != 0 || pulled != pushed {
			t.Errorf("view %s: pull/push differ (c1=%d c2=%d)\npulled:\n%s\npushed:\n%s", view, c1, c2, pulled, pushed)
		}
	}

	// 4. Non-existent variable fails with error
	_, errbMissing, codeMissing := runToolWithEnv(t, dir, "", fakeEnv, "-t", "paths", "env:DOES_NOT_EXIST")
	if codeMissing == 0 || !strings.Contains(errbMissing, "file does not exist") && !strings.Contains(errbMissing, "does not exist") && !strings.Contains(errbMissing, "no such") {
		t.Fatalf("missing var should fail: %d %q", codeMissing, errbMissing)
	}

	// 5. Live host environment through standard runTool (rc.Env == nil)
	liveOut, liveErr, liveCode := runTool(t, dir, "", "-t", "paths", "env:")
	if liveCode != 0 || liveErr != "" {
		t.Fatalf("live env paths: %d %q", liveCode, liveErr)
	}
	if !strings.HasPrefix(liveOut, "env:\n") {
		t.Fatalf("live env output missing root: %q", liveOut)
	}
	if !strings.Contains(liveOut, "env:PATH") {
		t.Errorf("live env output expected to contain env:PATH, got:\n%s", liveOut)
	}
}
