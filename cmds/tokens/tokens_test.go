package tokenscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func runTokens(t *testing.T, dir, stdin string, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &o, Err: &e},
	}
	code = cmd.Run(rc, args)
	return o.String(), e.String(), code
}

func TestEstimateMonotonic(t *testing.T) {
	if estimate(0, 0) != 0 {
		t.Errorf("empty should be 0, got %d", estimate(0, 0))
	}
	if estimate(400, 60) < 90 {
		t.Errorf("400 chars should estimate ~100 tokens, got %d", estimate(400, 60))
	}
	// more input ⇒ not fewer tokens
	if estimate(800, 120) <= estimate(400, 60) {
		t.Error("estimate should grow with input")
	}
}

func TestTokensStdin(t *testing.T) {
	out, _, code := runTokens(t, t.TempDir(), "one two three four")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "-") { // stdin shows as "-"
		t.Errorf("stdin tally should label '-': %q", out)
	}
}

func TestTokensFilesAndTotalJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("the quick brown fox jumps"), 0o644)

	out, _, code := runTokens(t, dir, "", "--json", "a.txt", "b.txt")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var env struct {
		Schema string `json:"schema_version"`
		Files  []struct {
			Name   string `json:"file"`
			Tokens int    `json:"est_tokens"`
		} `json:"files"`
		Total struct {
			Tokens int `json:"est_tokens"`
		} `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if env.Schema != tokensSchemaVersion || len(env.Files) != 2 {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Total.Tokens != env.Files[0].Tokens+env.Files[1].Tokens {
		t.Errorf("total %d != sum of files", env.Total.Tokens)
	}
}

func TestTokensMissingFileLoud(t *testing.T) {
	_, errOut, code := runTokens(t, t.TempDir(), "", "nope.txt")
	if code == 0 || !strings.Contains(errOut, "nope.txt") {
		t.Errorf("missing file should error loudly: code=%d err=%q", code, errOut)
	}
}
