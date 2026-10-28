package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const sharpBody = "@guard(effects: \"read,write\")\n@effects(\"read,write\")\n@require('true')\n@ensure('test -f done')\nwork() { broken; }\nwork\n"

func sharpFixture(t *testing.T, runner BashSharpRunner) (*config, Skill, Source, *ProbeSet) {
	t.Helper()
	doc := "---\nname: sharp\ndescription: fixture\n---\n```bsh\n" + sharpBody + "```\n"
	cfg := &config{cfgDir: t.TempDir(), statics: map[string]string{"bashy": "test"}, bashSharpRunner: runner,
		sources: []Source{EmbedSource(fstest.MapFS{"sharp/SKILL.md": {Data: []byte(doc)}}, RingEmbedded)}}
	sk, src, ok := cfg.catalog().Get("sharp")
	if !ok {
		t.Fatal("missing fixture")
	}
	ps, _ := cfg.probes(false)
	return cfg, sk, src, ps
}

func TestBashSharpRun(t *testing.T) {
	for _, code := range []int{0, 3} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			t.Setenv("SKILL_RUN_ENV", "inherited")
			cwd := t.TempDir()
			cfg, sk, src, ps := sharpFixture(t, func(ctx context.Context, dir string, env []string, body string, out, errout io.Writer) (int, error) {
				if dir != cwd || body != sharpBody || !strings.Contains(strings.Join(env, "\n"), "SKILL_RUN_ENV=inherited") {
					t.Fatal("lost caller or body")
				}
				fmt.Fprint(out, "command output")
				return code, nil
			})
			var log strings.Builder
			rec, _, err := runSkill(cfg, sk, src, ps, cwd, &log)
			if err != nil || rec.Valid != (code == 0) || *rec.Status != code || rec.Identity != bashSharpIdentity(sharpBody) || rec.Tier != "bashy@test" || rec.ContextKey == "" {
				t.Fatalf("%+v %v", rec, err)
			}
			if strings.Join(rec.Effects, ",") != "read,write" || log.String() != "command output" {
				t.Fatalf("%+v %s", rec, log.String())
			}
			data, err := os.ReadFile(filepath.Join(cfg.cfgDir, "attest", "sharp.jsonl"))
			var saved AttestRecord
			if err != nil || json.Unmarshal(data, &saved) != nil || saved.Identity != rec.Identity || saved.Valid != rec.Valid {
				t.Fatalf("receipt %s %v", data, err)
			}
			f := &cobraRunner{t: t, opts: []Option{WithConfigDir(cfg.cfgDir), WithSource(src), WithBashSharpRunner(cfg.bashSharpRunner)}}
			// The CLI uses the actual caller directory.
			cfg.bashSharpRunner = func(context.Context, string, []string, string, io.Writer, io.Writer) (int, error) { return code, nil }
			f.opts[2] = WithBashSharpRunner(cfg.bashSharpRunner)
			stdout, _, err := f.run("run", "sharp", "--json")
			if (err == nil) != (code == 0) || !strings.Contains(stdout, `"identity":"sha256:`) {
				t.Fatalf("CLI: %s %v", stdout, err)
			}
		})
	}
}

func TestBashSharpAdapt(t *testing.T) {
	for _, mutation := range []string{"steps", "ensure", "guard", "effects", "require", "fails"} {
		t.Run(mutation, func(t *testing.T) {
			calls := 0
			cfg, sk, src, ps := sharpFixture(t, func(_ context.Context, _ string, _ []string, body string, out, errout io.Writer) (int, error) {
				calls++
				if strings.Contains(body, "fixed") {
					return 0, nil
				}
				fmt.Fprintln(errout, "failure detail")
				return 3, nil
			})
			fix := strings.ReplaceAll(sharpBody, "broken", "fixed")
			if mutation == "fails" {
				fix = sharpBody
			}
			if mutation != "steps" && mutation != "fails" {
				fix = strings.Replace(fix, "@"+mutation+"(", "@"+mutation+"( ", 1)
			}
			complete := func(prompt string) (string, error) {
				if !strings.Contains(prompt, "SKILL.md:") || !strings.Contains(prompt, "failure detail") {
					t.Fatal(prompt)
				}
				return "```bashsharp\n" + fix + "```\n", nil
			}
			var log strings.Builder
			rec, outcome, err := adaptiveRun(cfg, sk, src, ps, t.TempDir(), &log, complete, 1)
			if mutation != "steps" {
				if err == nil || outcome != OutcomeFailed {
					t.Fatalf("accepted: %+v %s %v", rec, outcome, err)
				}
				want := 1
				if mutation == "fails" {
					want = 2
				}
				if calls != want {
					t.Fatalf("candidate ran %d", calls)
				}
				p, _ := prepareRun(cfg, sk, src, ps)
				if _, err := os.Stat(bashSharpOverlay(cfg, p)); !os.IsNotExist(err) {
					t.Fatal("saved rejected overlay")
				}
				return
			}
			if err != nil || outcome != OutcomeRepaired || !rec.Valid {
				t.Fatalf("%+v %s %v", rec, outcome, err)
			}
			rec, _, err = runSkill(cfg, sk, src, ps, t.TempDir(), &log)
			if err != nil || !rec.Valid || !strings.Contains(log.String(), "running the host's learned version") {
				t.Fatalf("overlay: %+v %v %s", rec, err, log.String())
			}
			doc, _ := src.File(sk.Name, "SKILL.md")
			if !strings.Contains(string(doc), "broken") {
				t.Fatal("authored source changed")
			}
			data, _ := os.ReadFile(filepath.Join(cfg.cfgDir, "attest", "sharp.jsonl"))
			if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 3 {
				t.Fatalf("missing run receipts: %s", data)
			}
		})
	}
}

func TestBashSharpMissingRunnerAndFences(t *testing.T) {
	cfg, sk, src, ps := sharpFixture(t, nil)
	if _, err := prepareRun(cfg, sk, src, ps); err == nil || !strings.Contains(err.Error(), "no Bash# runner in this host") {
		t.Fatal(err)
	}
	doc := "```text\n```bsh\nignored\n```\n```bsh\nfirst\n```\nprose\n```bashsharp\nsecond\n```\n"
	if got := bashSharpFences(doc); got != "first\nsecond\n" {
		t.Fatalf("fences: %q", got)
	}
}
