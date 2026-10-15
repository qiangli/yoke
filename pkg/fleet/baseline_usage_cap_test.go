package fleet

import (
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/policy/advice"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// TestBaselineUsageProbeEffectsCoverCommands (story 765d90d1d34f) is the
// static seat-belt for the live failure that story caught: a seed whose
// declared effects do not cover the atlas effects of the commands its body
// runs dies 126 under the snippet runner's @guard cap
// ("dag: effect cap denied ..."). The runner turns the probe's Effects:
// into advice.WithCap and classifies every command the body dispatches
// through the atlas, so the seed and the atlas can drift apart silently —
// a fake-fetch test passes while the real probe refuses to run.
//
// For EVERY compiled-in baseline usage probe (models and plans), parse the
// body with the same Bash# parser the runner uses, collect each CallExpr's
// first word, and assert advice.ParseCap(declared).Exceeded(atlas effects)
// is empty. Unknown commands fail the test, not open: the runner's guard
// denies them as "unknown". Shell builtins the interpreter executes
// itself (exit, [, echo, ...) never reach the cap seam and are skipped —
// interp.IsBuiltin is the same predicate the runtime dispatch consults.
func TestBaselineUsageProbeEffectsCoverCommands(t *testing.T) {
	type record struct {
		kind   string
		name   string
		probes []UsageProbe
	}
	var records []record
	for _, dir := range []string{"baseline/models", "baseline/plans"} {
		files, err := fs.Glob(baselineFS, dir+"/*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no baseline seeds under %s", dir)
		}
		for _, f := range files {
			body, err := baselineFS.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			name := strings.TrimSuffix(path.Base(f), ".yaml")
			switch dir {
			case "baseline/models":
				m, err := ParseModel(name, body, nil)
				if err != nil {
					t.Fatalf("%s: %v", f, err)
				}
				records = append(records, record{"model", m.Name, m.Usage})
			default:
				p, err := ParsePlan(name, body, nil)
				if err != nil {
					t.Fatalf("%s: %v", f, err)
				}
				records = append(records, record{"plan", p.Name, p.Usage})
			}
		}
	}

	checked := 0
	for _, r := range records {
		for _, p := range r.probes {
			checked++
			where := r.kind + ":" + r.name + "/" + p.ID
			if len(p.Run.Effects) == 0 {
				t.Errorf("%s: no declared effects", where)
				continue
			}
			cap, err := advice.ParseCap(strings.Join(p.Run.Effects, ","))
			if err != nil {
				t.Errorf("%s: effects %v: %v", where, p.Run.Effects, err)
				continue
			}
			prog, err := syntax.NewParser(syntax.Variant(syntax.LangBashPP)).Parse(strings.NewReader(p.Run.Body), "probe.bsh")
			if err != nil {
				t.Errorf("%s: parse body: %v", where, err)
				continue
			}
			syntax.Walk(prog, func(n syntax.Node) bool {
				call, ok := n.(*syntax.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				word := call.Args[0]
				lit := word.Lit()
				if lit == "" {
					var buf strings.Builder
					_ = syntax.NewPrinter().Print(&buf, word)
					t.Errorf("%s: command word %s is not a plain literal", where, buf.String())
					return true
				}
				name := lit
				if i := strings.LastIndexAny(name, `/\`); i >= 0 {
					name = name[i+1:]
				}
				if interp.IsBuiltin(name) {
					return true
				}
				entry, ok := atlas.Lookup(name)
				if !ok {
					t.Errorf("%s: command %q is not in the atlas (the guard denies it as unknown)", where, name)
					return true
				}
				if denied := cap.Exceeded(entry.Effects); len(denied) > 0 {
					t.Errorf("%s: command %q needs effects %s not declared (declared: %s; atlas: %s)",
						where, name, strings.Join(denied, ","), cap, strings.Join(entry.Effects, ","))
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("no baseline usage probes found")
	}
}
