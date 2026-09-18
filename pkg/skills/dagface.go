package skills

// The tasks face: a skill folder MAY carry tasks.md — a dag task file
// (### headings = targets with Requires:/Sources:/Effects: metadata).
// `skills run NAME --target T` executes one target through the dag
// engine. The line that matters here is executability, not markdown
// dialect: SKILL.md stays model-facing prose; tasks.md is the
// machine-executable face; skill.dhnt is the verification spine. For a
// contracted skill the target runs AS the steps phase — a synthetic
// step bound to the dag engine — so the contract, the effect cap, the
// pre-flight audit, and the attestation all apply unchanged.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	dhntskills "github.com/dhnt/dhnt/skills"
	"github.com/qiangli/yoke/pkg/dag"
)

// dagPrimitive is the synthetic dhnt primitive id a --target run binds
// (canonical dhnt: da-ga).
const dagPrimitive = "daga"

// materializeTasks resolves the skill's tasks face to an on-disk path:
// a bundled tasks.md wins (the real file for directory-backed skills, a
// temp file for embedded ones); otherwise a `metadata.tasks` POINTER —
// a repo-relative path resolved under cwd, so a skill can package
// discovery/gating/attestation for a dag file the repo already has
// (every dag file is a latent skill). The returned document comes from
// the dag engine's own parser, so target names match execution exactly.
func materializeTasks(sk Skill, src Source, cwd string) (path string, doc *dag.Document, cleanup func(), err error) {
	cleanup = func() {}
	switch data, ok := src.File(sk.Name, "tasks.md"); {
	case ok && sk.Dir != "":
		path = filepath.Join(sk.Dir, "tasks.md")
	case ok:
		f, err := os.CreateTemp("", "bashy-skill-"+sk.Name+"-*.md")
		if err != nil {
			return "", nil, cleanup, err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return "", nil, cleanup, err
		}
		if err := f.Close(); err != nil {
			return "", nil, cleanup, err
		}
		path = f.Name()
		cleanup = func() { _ = os.Remove(f.Name()) }
	case sk.Meta["tasks"] != "":
		rel := filepath.FromSlash(sk.Meta["tasks"])
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			return "", nil, cleanup, fmt.Errorf("skills: %q metadata tasks pointer %q must be a repo-relative path (no absolute, no ..)", sk.Name, sk.Meta["tasks"])
		}
		path = filepath.Join(cwd, rel)
		if _, err := os.Stat(path); err != nil {
			return "", nil, cleanup, fmt.Errorf("skills: %q tasks pointer %q not found under %s", sk.Name, sk.Meta["tasks"], cwd)
		}
	default:
		return "", nil, cleanup, fmt.Errorf("skills: %q has no tasks face (bundle a tasks.md, or point at one via metadata tasks: <repo-relative path>)", sk.Name)
	}
	doc, err = dag.ParseFile(path)
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("skills: %q tasks face: %w", sk.Name, err)
	}
	return path, doc, cleanup, nil
}

// effectAtoms maps dag `Effects:` vocabulary to the dhnt lattice.
var effectAtoms = map[string]dhntskills.Effect{
	"read": dhntskills.EffRead, "write": dhntskills.EffWrite,
	"net": dhntskills.EffNet, "spend": dhntskills.EffSpend,
	"destroy": dhntskills.EffDestroy, "time": dhntskills.EffTime,
}

// targetEffects computes the effects a target run will report, from the
// dag file's own declarations: the union of `Effects:` over the
// target's transitive Requires closure (dependencies run too), plus the
// implied read. If ANY task in the closure declares nothing, the whole
// run falls back to the conservative read+write convention — declared
// effects narrow the audit only when the declaration is complete.
// An unknown atom is a loud error, never a guess.
func targetEffects(doc *dag.Document, target string) ([]dhntskills.Effect, error) {
	byName := map[string]*dag.Task{}
	for _, t := range doc.Tasks {
		byName[t.Name] = t
	}
	conservative := []dhntskills.Effect{dhntskills.EffRead, dhntskills.EffWrite}
	seen := map[string]bool{}
	atoms := map[dhntskills.Effect]bool{dhntskills.EffRead: true}
	var walk func(name string) (bool, error)
	walk = func(name string) (bool, error) {
		if seen[name] {
			return true, nil
		}
		seen[name] = true
		t, ok := byName[name]
		if !ok || len(t.Effects) == 0 {
			return false, nil // unknown dep or undeclared task ⇒ conservative
		}
		for _, a := range t.Effects {
			eff, ok := effectAtoms[strings.ToLower(strings.TrimSpace(a))]
			if !ok {
				return false, fmt.Errorf("skills: target %q declares unknown effect %q (know: read write net spend destroy time)", name, a)
			}
			atoms[eff] = true
		}
		for _, dep := range t.Requires {
			complete, err := walk(dep)
			if err != nil || !complete {
				return complete, err
			}
		}
		return true, nil
	}
	complete, err := walk(target)
	if err != nil {
		return nil, err
	}
	if !complete {
		return conservative, nil
	}
	out := make([]dhntskills.Effect, 0, len(atoms))
	for e := range atoms {
		out = append(out, e)
	}
	return out, nil
}

// runDagTarget executes one target of a task file through the dag
// engine (the same engine as `bashy dag`), streaming output to log.
//
// `bashy dag` runs bodies in the INVOKING cwd (make parity, Sprint 185); a
// skill's tasks.md has always run in the directory holding the task file (the
// skill dir for an embedded tasks.md, the project for a `tasks:` pointer
// materialized under cwd), so that directory is entered for the duration of
// the run. Whether skill tasks should instead act in the project cwd is a
// separate decision — this preserves the shipped behaviour, nothing more.
func runDagTarget(tasksPath, target string, log io.Writer) error {
	dir := filepath.Dir(tasksPath)
	prev, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("skills: dag target %q: %w", target, err)
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("skills: dag target %q: %w", target, err)
	}
	defer os.Chdir(prev)
	cmd := dag.NewDagCmd()
	cmd.SetArgs([]string{tasksPath, target})
	cmd.SetOut(log)
	cmd.SetErr(log)
	if err := cmd.Execute(); err != nil {
		return fmt.Errorf("skills: dag target %q: exit %d: %w", target, dag.ExitCodeOf(err), err)
	}
	return nil
}

// runTargetSkill is the --target engine. Contracted skills run the
// target as the steps phase (attested, cap-audited); uncontracted ones
// run it plainly (rung "tasks": executable, not yet verifiable).
func runTargetSkill(cfg *config, sk Skill, src Source, ps *ProbeSet, target, cwd string, log io.Writer) (AttestRecord, bool, error) {
	if v := verdictOf(sk, ps); !v.Applicable {
		return AttestRecord{}, false, fmt.Errorf("skills: %q is not applicable here (%s)", sk.Name, v.Failing)
	}
	tasksPath, doc, cleanup, err := materializeTasks(sk, src, cwd)
	defer cleanup()
	if err != nil {
		return AttestRecord{}, false, err
	}
	known := false
	for _, t := range doc.Order {
		if t == target {
			known = true
			break
		}
	}
	if !known {
		return AttestRecord{}, false, fmt.Errorf("skills: %q has no target %q (targets: %v)", sk.Name, target, doc.Order)
	}

	if !sk.Dhnt.Valid() {
		// Executable but uncontracted: run the target, report plainly.
		return AttestRecord{}, false, runDagTarget(tasksPath, target, log)
	}

	// Contracted: the dag target IS the steps phase. The audit uses the
	// dag file's own Effects: declarations (union over the target's
	// Requires closure) when complete, else the read+write convention —
	// so a pure-check target under a read-only cap is runnable, and a
	// declared `destroy` refuses under any normal cap.
	effects, err := targetEffects(doc, target)
	if err != nil {
		return AttestRecord{}, false, err
	}
	p, err := prepareRun(cfg, sk, src, ps)
	if err != nil {
		return AttestRecord{}, false, err
	}
	p.preflight = effects
	p.prims = map[string]dhntskills.PrimitiveFn{
		dagPrimitive: func([]dhntskills.Arg) ([]dhntskills.Effect, error) {
			// Observed = the same set the audit used: the receipt never
			// claims less than the audit demanded.
			if err := runDagTarget(tasksPath, target, log); err != nil {
				return effects, err
			}
			return effects, nil
		},
	}
	effective := dhntskills.Skill{
		Name:      p.base.Name,
		Caps:      p.base.Caps,
		EffectCap: p.base.EffectCap,
		Contract:  p.base.Contract,
		// Step name must be canonical dhnt too — the human-facing target
		// name travels in the receipt's outcome, not the canonical form.
		Steps: []dhntskills.Step{{Name: dagPrimitive, Primitive: dagPrimitive}},
	}
	rec, err := runEffective(sk.Name, effective, p, cwd, log)
	if err != nil {
		return AttestRecord{}, false, err
	}
	storeAttest(cfg, rec, log)
	return rec, true, nil
}
