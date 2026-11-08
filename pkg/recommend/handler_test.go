package recommend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/fleet"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func resetGuidance(t *testing.T) {
	t.Helper()
	guidanceSession.Lock()
	guidanceSession.scripts = map[string]int{}
	guidanceSession.emitted = map[string]bool{}
	guidanceSession.Unlock()
	t.Setenv("BASHY_HINTS", "on")
	t.Setenv("BASHY_AGENTIC", "1")
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_COMMANDS_DIR", t.TempDir())
	t.Setenv("BASHY_SKILLS_DIR", t.TempDir())
}

func runGuidance(t *testing.T, args []string, diagnostic string, exit error) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	next := func(ctx context.Context, got []string) error {
		if !reflect.DeepEqual(got, args) {
			t.Fatalf("argv changed: %q", got)
		}
		hc := interp.HandlerCtx(ctx)
		fmt.Fprint(hc.Stdout, "unchanged stdout")
		fmt.Fprint(hc.Stderr, diagnostic)
		return exit
	}
	wrapped := Handler()(next)
	runner, err := interp.New(interp.StdIO(nil, &stdout, &stderr), interp.ExecHandler(func(ctx context.Context, _ []string) error {
		got := wrapped(ctx, args)
		if got != exit {
			t.Fatalf("error changed: %v -> %v", exit, got)
		}
		return got
	}))
	if err != nil {
		t.Fatal(err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader("fixture-command"), "")
	if err != nil {
		t.Fatal(err)
	}
	_ = runner.Run(context.Background(), file)
	if stdout.String() != "unchanged stdout" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), diagnostic) {
		t.Fatalf("stderr lost: %q", stderr.String())
	}
	return stdout.String(), strings.TrimSpace(strings.TrimPrefix(stderr.String(), diagnostic))
}

func decodeGuidance(t *testing.T, hint string) Guidance {
	t.Helper()
	var g Guidance
	if err := json.Unmarshal([]byte(hint), &g); err != nil {
		t.Fatalf("guidance %q: %v", hint, err)
	}
	if g.Schema != GuidanceSchema {
		t.Fatalf("schema = %q", g.Schema)
	}
	return g
}

func TestHandlerCases(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		code       int
		diagnostic string
		kind       Kind
	}{
		{"missing", []string{"fixture-absent"}, 127, "fixture-absent: not found", KindRegister},
		{"missing diagnostic", []string{"fixture-absent"}, 1, "fixture-absent: command not found\n", KindRegister},
		{"usage", []string{"bashy", "app"}, 2, "panic: usage error", KindCorrect},
		{"workspace", []string{"bashy", "app"}, 3, "app workspace not initialized; internal error", KindCorrect},
		{"prerequisite", []string{"bashy", "app"}, 3, "missing prerequisite", KindCorrect},
		{"transient", []string{"bashy", "fetch"}, 7, "connection refused; panic:", KindCorrect},
		{"defect", []string{"bashy", "app"}, 3, "failed", KindShape},
		{"panic", []string{"bashy", "app"}, 1, "panic: broken", KindShape},
		{"internal", []string{"bashy", "app"}, 1, "internal-error", KindShape},
		{"ordinary", []string{"bashy", "app"}, 1, "failed", ""},
		{"refusal", []string{"bashy", "app"}, 125, "refused", ""},
		{"external", []string{"foreign"}, 3, "panic: broken", ""},
		{"shell mode", []string{"bashy", "-c", "false"}, 3, "failed", ""},
		{"success", []string{"bashy", "app"}, 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetGuidance(t)
			var err error
			if tc.code != 0 {
				err = interp.NewExitStatus(uint8(tc.code))
			}
			_, hint := runGuidance(t, tc.args, tc.diagnostic, err)
			if tc.kind == "" {
				if hint != "" {
					t.Fatalf("unexpected hint %q", hint)
				}
				return
			}
			g := decodeGuidance(t, hint)
			if g.Kind != tc.kind || g.Executable {
				t.Fatalf("guidance = %+v", g)
			}
			if g.Kind == KindShape && (g.Authorization != AuthApproval || !reflect.DeepEqual(g.Command[:4], []string{"bashy", "shape", "wish", "bashy"})) {
				t.Fatalf("shape = %+v", g)
			}
			_, hint = runGuidance(t, tc.args, tc.diagnostic, err)
			if hint != "" {
				t.Fatalf("duplicate: %q", hint)
			}
		})
	}
}

func TestHandlerRepeatScript(t *testing.T) {
	for _, interpreter := range []string{"", "bash"} {
		t.Run("interpreter="+interpreter, func(t *testing.T) {
			resetGuidance(t)
			path := filepath.Join(t.TempDir(), "fixture-repeat.sh")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{path}
			if interpreter != "" {
				args = []string{interpreter, path}
			}
			for i := 1; i <= 4; i++ {
				_, hint := runGuidance(t, args, "", nil)
				if i != 3 {
					if hint != "" {
						t.Fatalf("run %d: %q", i, hint)
					}
					continue
				}
				g := decodeGuidance(t, hint)
				want := []string{"bashy", "command", "add", "fixture-repeat", "--set", "exec.0=" + path}
				if g.Kind != KindRegister || !reflect.DeepEqual(g.Command, want) || g.Verify != "bashy commands verify fixture-repeat" || g.Executable {
					t.Fatalf("guidance: %+v", g)
				}
			}
		})
	}
}

func TestHandlerReuse(t *testing.T) {
	for _, name := range []string{"fixture-capability", "fixture-alias", "fixture-skill"} {
		t.Run(name, func(t *testing.T) {
			resetGuidance(t)
			cmd := fleet.NewCommandsCmd()
			cmd.SetArgs([]string{"add", "fixture-capability", "--set", "exec.0=echo", "--set", "aliases.0=fixture-alias"})
			cmd.SetOut(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			// A filesystem skill source, using the existing catalog's native format.
			dir := filepath.Join(os.Getenv("BASHY_SKILLS_DIR"), "fixture-skill")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: fixture-skill\ndescription: fixture\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, hint := runGuidance(t, []string{name}, "not found", interp.NewExitStatus(127))
			g := decodeGuidance(t, hint)
			if g.Kind != KindReuse || !strings.Contains(g.Reason, "fixture-") {
				t.Fatalf("guidance: %+v", g)
			}
		})
	}
}

func TestHandlerSuppressionAndErrorIdentity(t *testing.T) {
	resetGuidance(t)
	t.Setenv("BASHY_HINTS", "off")
	err := errors.New("internal error")
	_, hint := runGuidance(t, []string{"bashy", "app"}, "unchanged stderr", err)
	if hint != "" {
		t.Fatalf("suppressed hint: %q", hint)
	}
	t.Setenv("BASHY_HINTS", "on")
	_, hint = runGuidance(t, []string{"bashy", "app"}, "", err)
	if decodeGuidance(t, hint).Kind != KindShape {
		t.Fatal("suppression consumed dedup")
	}
	// Different kinds for the same key are independently deduplicated.
	_, hint = runGuidance(t, []string{"bashy", "app"}, "usage", interp.NewExitStatus(2))
	if decodeGuidance(t, hint).Kind != KindCorrect {
		t.Fatal("dedup crossed kinds")
	}
}

func TestGuidancePolicy(t *testing.T) {
	p := Policy()
	if p.ScriptThreshold != 3 || p.DedupScope != "process-session" || p.SuppressionEnv != "BASHY_HINTS=off" {
		t.Fatalf("policy: %+v", p)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Fatal(err)
	}
	p.ShapeExcludedExits[0] = 99
	if Policy().ShapeExcludedExits[0] != 0 {
		t.Fatal("policy mutable")
	}
}

func TestHandlerHumanLine(t *testing.T) {
	resetGuidance(t)
	t.Setenv("BASHY_AGENTIC", "")
	old := weavecli.DetectTool
	weavecli.DetectTool = func() (string, bool) { return "", false }
	t.Cleanup(func() { weavecli.DetectTool = old })
	_, hint := runGuidance(t, []string{"bashy", "app"}, "", interp.NewExitStatus(3))
	g, _, ok := commandGuidance("", []string{"bashy", "app"}, 3, "")
	if !ok || hint != strings.TrimSpace(g.Line()) {
		t.Fatalf("human hint = %q", hint)
	}
}
