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

	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/shape/verify/repair"
	"github.com/qiangli/yoke/pkg/unit"
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
			if err != nil || rec.Valid != (code == 0) || *rec.Status != code || rec.Identity != unit.BodyIdentity([]byte(sharpBody)) || rec.Capability == "" || rec.Tier != "bashy@test" || rec.ContextKey == "" {
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
			if (err == nil) != (code == 0) || !strings.Contains(stdout, `"identity":"h`) {
				t.Fatalf("CLI: %s %v", stdout, err)
			}
		})
	}
}

func TestBashSharpAdapt(t *testing.T) {
	for _, mutation := range []string{"steps", "ensure", "guard", "effects", "require", "fails"} {
		t.Run(mutation, func(t *testing.T) {
			calls := 0
			cfg, sk, src, ps := sharpFixture(t, func(_ context.Context, dir string, env []string, body string, out, errout io.Writer) (int, error) {
				calls++
				if strings.Contains(body, "fixed") {
					// A genuine repair: the host interpreter records the
					// contracted call and the fixed steps produce their
					// effect. Exit 0 alone never promotes — without the
					// call and the effect the repair is unverified.
					old, had := os.LookupEnv(repair.CallLogEnv)
					for _, e := range env {
						if v, ok := strings.CutPrefix(e, repair.CallLogEnv+"="); ok {
							os.Setenv(repair.CallLogEnv, v)
						}
					}
					repair.RecordCall("work")
					if had {
						os.Setenv(repair.CallLogEnv, old)
					} else {
						os.Unsetenv(repair.CallLogEnv)
					}
					os.WriteFile(filepath.Join(dir, "done"), []byte{}, 0o644)
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

const verifiedDecorators = "@guard(effects: \"read,write\")\n@effects(\"read,write\")\n@require('true')\n@ensure('test -f done')\n"

// verifiedRunner stands in for the host's decorator-aware interpreter: it
// records contracted-function invocations through the call log the run
// carries (the accounting the verifier checks), performs the file effect
// the repaired steps claim, and fails bodies that still carry the bug.
func verifiedRunner(calls *int, seen *[]string) BashSharpRunner {
	return func(_ context.Context, dir string, env []string, body string, out, errout io.Writer) (int, error) {
		*calls++
		*seen = append(*seen, body)
		var logPath string
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, repair.CallLogEnv+"="); ok {
				logPath = v
			}
		}
		// Bridge the run's call log into the process environment the way
		// the real interpreter receives it, so RecordCall accounts here.
		old, had := os.LookupEnv(repair.CallLogEnv)
		if logPath == "" {
			os.Unsetenv(repair.CallLogEnv)
		} else {
			os.Setenv(repair.CallLogEnv, logPath)
		}
		defer func() {
			if had {
				os.Setenv(repair.CallLogEnv, old)
			} else {
				os.Unsetenv(repair.CallLogEnv)
			}
		}()
		for _, line := range strings.Split(body, "\n") {
			if strings.TrimSpace(line) == "work" {
				repair.RecordCall("work")
			}
		}
		if strings.Contains(body, "broken") {
			fmt.Fprintln(errout, "failure detail")
			return 3, nil
		}
		if strings.Contains(body, "touch done") {
			if err := os.WriteFile(filepath.Join(dir, "done"), []byte{}, 0o644); err != nil {
				return 1, err
			}
		}
		return 0, nil
	}
}

// promoteGenuine runs one repair round with the genuine fix and returns the
// fixture alongside the promoted overlay body.
func promoteGenuine(t *testing.T, calls *int, seen *[]string) (*config, Skill, Source, *ProbeSet, string, string) {
	t.Helper()
	cfg, sk, src, ps := sharpFixture(t, verifiedRunner(calls, seen))
	genuine := verifiedDecorators + "work() { touch done; }\nwork\n"
	complete := func(prompt string) (string, error) {
		return "```bashsharp\n" + genuine + "```\n", nil
	}
	var log strings.Builder
	rec, outcome, err := adaptiveRun(cfg, sk, src, ps, t.TempDir(), &log, complete, 1)
	if err != nil || outcome != OutcomeRepaired || !rec.Valid {
		t.Fatalf("genuine repair refused: %+v %s %v", rec, outcome, err)
	}
	return cfg, sk, src, ps, genuine, log.String()
}

// Only a verified repair reaches the overlay: a weakened @ensure, an
// early exit and an uncalled contract are refused, and the genuine repair
// is promoted, sealed under its identity, and runs next time.
func TestBashSharpOverlayVerified(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		reason  string
		promote bool
	}{
		{"weakened ensure", strings.Replace(sharpBody, "@ensure('test -f done')", "@ensure('true')", 1), "changed decorator", false},
		{"early exit", verifiedDecorators + "work() { :; }\nwork\n", "postcondition", false},
		{"uncalled contract", verifiedDecorators + "work() { :; }\ntouch done\n", "never called", false},
		{"genuine repair", verifiedDecorators + "work() { touch done; }\nwork\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var seen []string
			cfg, sk, src, ps := sharpFixture(t, verifiedRunner(&calls, &seen))
			complete := func(prompt string) (string, error) {
				return "```bashsharp\n" + tc.body + "```\n", nil
			}
			var log strings.Builder
			rec, outcome, err := adaptiveRun(cfg, sk, src, ps, t.TempDir(), &log, complete, 1)
			p, perr := prepareRun(cfg, sk, src, ps)
			if perr != nil {
				t.Fatal(perr)
			}
			overlay := bashSharpOverlay(cfg, p)
			if !tc.promote {
				if err == nil || outcome != OutcomeFailed {
					t.Fatalf("promoted an unverified repair: %+v %s %v", rec, outcome, err)
				}
				if !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("err = %v, want %q", err, tc.reason)
				}
				if _, serr := os.Stat(overlay); !os.IsNotExist(serr) {
					t.Fatal("an unverified repair reached the overlay")
				}
				return
			}
			if err != nil || outcome != OutcomeRepaired || !rec.Valid {
				t.Fatalf("verified repair refused: %+v %s %v", rec, outcome, err)
			}
			raw, serr := os.ReadFile(overlay)
			if serr != nil || string(raw) != tc.body {
				t.Fatalf("overlay = %q, %v", raw, serr)
			}
			// The pass is sealed in an evidence record bound to the
			// overlay body's identity.
			if _, serr := os.Stat(evidence.Path(cfg.cfgDir, unit.BodyIdentity([]byte(tc.body)))); serr != nil {
				t.Fatalf("no evidence bound to the overlay identity: %v", serr)
			}
			// Next time the learned version runs.
			var again strings.Builder
			rec2, _, err2 := runSkill(cfg, sk, src, ps, t.TempDir(), &again)
			if err2 != nil || !rec2.Valid || !strings.Contains(again.String(), "running the host's learned version") {
				t.Fatalf("overlay rerun: %+v %v %s", rec2, err2, again.String())
			}
			if seen[len(seen)-1] != tc.body {
				t.Fatal("the rerun did not execute the overlay body")
			}
		})
	}

	t.Run("stale overlay runs the authored unit", func(t *testing.T) {
		var calls int
		var seen []string
		cfg, sk, src, ps, _, _ := promoteGenuine(t, &calls, &seen)
		p, perr := prepareRun(cfg, sk, src, ps)
		if perr != nil {
			t.Fatal(perr)
		}
		tampered := verifiedDecorators + "work() { :; }\nwork\n"
		if err := os.WriteFile(bashSharpOverlay(cfg, p), []byte(tampered), 0o644); err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		rec, _, err := runSkill(cfg, sk, src, ps, t.TempDir(), &log)
		if err != nil || rec.Valid || *rec.Status != 3 || strings.Contains(log.String(), "learned version") {
			t.Fatalf("stale overlay ran: %+v %v %s", rec, err, log.String())
		}
		if seen[len(seen)-1] != sharpBody {
			t.Fatal("a stale overlay shadowed the authored unit")
		}
	})

	t.Run("missing evidence runs the authored unit", func(t *testing.T) {
		var calls int
		var seen []string
		cfg, sk, src, ps, _, _ := promoteGenuine(t, &calls, &seen)
		if err := os.RemoveAll(filepath.Join(cfg.cfgDir, "src")); err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		rec, _, err := runSkill(cfg, sk, src, ps, t.TempDir(), &log)
		if err != nil || rec.Valid || *rec.Status != 3 || strings.Contains(log.String(), "learned version") {
			t.Fatalf("overlay without evidence ran: %+v %v %s", rec, err, log.String())
		}
		if seen[len(seen)-1] != sharpBody {
			t.Fatal("an overlay without evidence shadowed the authored unit")
		}
	})
}

func TestBashSharpMissingRunnerAndFences(t *testing.T) {
	cfg, sk, src, ps := sharpFixture(t, nil)
	if _, err := prepareRun(cfg, sk, src, ps); err == nil || !strings.Contains(err.Error(), "no Bash# runner in this host") {
		t.Fatal(err)
	}
	doc := "```text\n```bsh\nignored\n```\n```bsh\nfirst\n```\nprose\n```bashsharp\nsecond\n```\n"
	if got := unit.BashSharpBody(doc); got != "first\nsecond\n" {
		t.Fatalf("fences: %q", got)
	}
}
