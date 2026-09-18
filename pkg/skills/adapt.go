package skills

// P3 — the contribution loop. When a run fails at this coordinate, a
// repair agent (any headless agentic CLI) proposes corrected STEPS; the
// candidate is accepted only if it satisfies the ORIGINAL contract
// within the ORIGINAL effect cap (the model may change the how, never
// the what); a passing fix is folded into a guarded environment arm and
// saved to the host-local overlay, so the next run — by any agent brand
// on this host — reuses it. The shared catalog is never touched here:
// pushing a learned version upstream is `promote`, a human-reviewed
// bundle, never an automatic write.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	dhntskills "github.com/dhnt/dhnt/skills"
)

// versions returns the host-local overlay store (nil without a store
// dir). Keyed by the BASE skill's identity, so overlays survive skill
// reinstalls and never collide across skills.
func (c *config) versions() dhntskills.VersionStore {
	if c.cfgDir == "" {
		return nil
	}
	return &dhntskills.FileVersionStore{Dir: filepath.Join(c.cfgDir, "versions")}
}

// --- learned bindings --------------------------------------------------
//
// A repair may introduce new step primitives, which need concrete
// commands. Those learned bindings live BESIDE the skill, in the ring-1
// store (<store>/bindings/<name>.json) — never spliced into the authored
// SKILL.md. bindEnv merges them over the frontmatter metadata.

func bindingsPath(storeDir, name string) string {
	return filepath.Join(storeDir, "bindings", name+".json")
}

func loadBindings(storeDir, name string) map[string]string {
	if storeDir == "" {
		return nil
	}
	data, err := os.ReadFile(bindingsPath(storeDir, name))
	if err != nil {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

func saveBindings(storeDir, name string, add map[string]string) error {
	if storeDir == "" || len(add) == 0 {
		return nil
	}
	m := loadBindings(storeDir, name)
	if m == nil {
		m = map[string]string{}
	}
	maps.Copy(m, add)
	p := bindingsPath(storeDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// --- the repair proposal -----------------------------------------------

// Completer proposes a corrected skill from a repair prompt — an agent
// CLI in production, a closure in tests.
type Completer = dhntskills.Completer

// execCompleter runs a headless agent CLI as the Completer: the prompt
// is appended as the final argument (`claude -p <prompt>`,
// `codex exec <prompt>`), stdout is the reply.
func execCompleter(command string) Completer {
	return func(prompt string) (string, error) {
		argv := strings.Fields(command)
		if len(argv) == 0 {
			return "", fmt.Errorf("skills: empty --repair-agent command")
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], prompt)...)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("skills: repair agent: %w", err)
		}
		return string(out), nil
	}
}

// repairPrompt asks for corrected steps in canonical dhnt plus their
// concrete command bindings. Glossary-free: the reply is parsed with
// ParseDhnt directly.
func repairPrompt(name string, effective dhntskills.Skill, meta map[string]string, failure string) string {
	canon, _ := dhntskills.LineariseDhnt(effective)
	var b strings.Builder
	fmt.Fprintf(&b, "A machine-checkable skill (%q) FAILED in this environment. Propose a fix.\n\n", name)
	b.WriteString("Rules:\n")
	b.WriteString("- You may change ONLY the steps (the how). The success contract and the\n")
	b.WriteString("  effect cap are enforced from the original no matter what you return.\n")
	b.WriteString("- Reply with the corrected skill in CANONICAL dhnt form inside <dhnt>…</dhnt>.\n")
	b.WriteString("  Grammar: sokilili <name> [efefecato <atoms> fini] [sotepo <step> <primitive> fini]… [enisure <predicate> fini]… fini\n")
	b.WriteString("  Every word is lowercase a-z only, and every consonant must be followed by a\n")
	b.WriteString("  vowel (e.g. `porota` is valid, `porta` is not). Keep the original name.\n")
	b.WriteString("- Each step primitive binds to a concrete shell command via metadata. For\n")
	b.WriteString("  every primitive you use, include a line `step-<primitive>: <command>` inside\n")
	b.WriteString("  <meta>…</meta> (existing bindings shown below may be reused as-is).\n\n")
	fmt.Fprintf(&b, "Failing skill (canonical): %s\n", canon)
	fmt.Fprintf(&b, "Failure: %s\n", failure)
	var bound []string
	for k, v := range meta {
		if strings.HasPrefix(k, "step-") || strings.HasPrefix(k, "check") {
			bound = append(bound, k+": "+v)
		}
	}
	sort.Strings(bound)
	if len(bound) > 0 {
		fmt.Fprintf(&b, "Current bindings:\n%s\n", strings.Join(bound, "\n"))
	}
	var preds, atoms []string
	for _, c := range effective.Contract {
		preds = append(preds, c.Predicate)
	}
	for _, e := range effective.EffectCap {
		atoms = append(atoms, e.String())
	}
	fmt.Fprintf(&b, "Contract that must still hold: %s\n", strings.Join(preds, " "))
	fmt.Fprintf(&b, "Allowed effects only: %s\n", strings.Join(atoms, " "))
	return b.String()
}

// parseRepairReply extracts the <dhnt> canonical block and the optional
// <meta> binding lines from an agent reply.
func parseRepairReply(reply string) (canon string, bindings map[string]string, err error) {
	block := func(tag string) (string, bool) {
		open, close := "<"+tag+">", "</"+tag+">"
		i := strings.Index(reply, open)
		if i < 0 {
			return "", false
		}
		j := strings.Index(reply[i+len(open):], close)
		if j < 0 {
			return "", false
		}
		return strings.TrimSpace(reply[i+len(open) : i+len(open)+j]), true
	}
	canon, ok := block("dhnt")
	if !ok || canon == "" {
		return "", nil, fmt.Errorf("skills: repair reply has no <dhnt> block")
	}
	bindings = map[string]string{}
	if metaBlock, ok := block("meta"); ok {
		for _, line := range strings.Split(metaBlock, "\n") {
			k, v, ok := strings.Cut(line, ":")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if ok && k != "" && v != "" && (strings.HasPrefix(k, "step-") || strings.HasPrefix(k, "check")) {
				bindings[k] = v
			}
		}
	}
	return canon, bindings, nil
}

// --- the adaptive run ----------------------------------------------------

// AdaptOutcome classifies how an adaptive run resolved.
type AdaptOutcome string

const (
	OutcomeBaseline AdaptOutcome = "baseline" // the skill passed as written
	OutcomeOverlay  AdaptOutcome = "overlay"  // a previously learned version passed
	OutcomeRepaired AdaptOutcome = "repaired" // a fresh fix was learned this run
	OutcomeFailed   AdaptOutcome = "failed"
)

// adaptiveRun is run --adapt: gate → prefer the host's learned version →
// on failure ask the repair agent for corrected steps, verify them under
// the ORIGINAL contract+cap, fold the fix into a guarded environment arm,
// and save it to the host overlay + learned bindings. Every executed
// shape is attested; the returned record is the final one.
func adaptiveRun(cfg *config, sk Skill, src Source, ps *ProbeSet, dir string, log io.Writer, complete Completer, attempts int) (AttestRecord, AdaptOutcome, error) {
	p, err := prepareRun(cfg, sk, src, ps)
	if err != nil {
		return AttestRecord{}, OutcomeFailed, err
	}
	vs := cfg.versions()

	effective, fromOverlay := p.base, false
	if vs != nil {
		if s, ok := dhntskills.ResolveLatest(vs, p.base); ok {
			effective, fromOverlay = s, true
			fmt.Fprintf(log, "skills: %s: trying the host's learned version first\n", sk.Name)
		}
	}

	rec, err := runEffective(sk.Name, effective, p, dir, log)
	if err == nil && rec.Attest.Valid {
		storeAttest(cfg, rec, log)
		if fromOverlay {
			return rec, OutcomeOverlay, nil
		}
		return rec, OutcomeBaseline, nil
	}
	failure := "the contract was not satisfied"
	if err != nil {
		failure = err.Error()
	} else {
		storeAttest(cfg, rec, log) // the failed baseline is evidence too
		if len(rec.Attest.Failed) > 0 {
			failure = "failed checks: " + strings.Join(rec.Attest.Failed, ", ")
		}
	}

	if complete == nil {
		return rec, OutcomeFailed, fmt.Errorf("skills: %q failed (%s) and no --repair-agent is configured", sk.Name, failure)
	}
	if attempts <= 0 {
		attempts = 2
	}
	lastErr := fmt.Errorf("%s", failure)
	for i := 0; i < attempts; i++ {
		reply, perr := complete(repairPrompt(sk.Name, effective, p.meta, failure))
		if perr != nil {
			lastErr = perr
			continue
		}
		canon, learned, perr := parseRepairReply(reply)
		if perr != nil {
			lastErr = perr
			continue
		}
		cand, perr := dhntskills.ParseDhnt(canon)
		if perr != nil {
			lastErr = fmt.Errorf("skills: repair candidate: %w", perr)
			continue
		}
		// Graft the ORIGINAL contract + cap onto the candidate's steps —
		// a model cannot pass by weakening the spec or raising the cap.
		verify := dhntskills.Skill{
			Name:      p.base.Name,
			Caps:      p.base.Caps,
			EffectCap: p.base.EffectCap,
			Contract:  p.base.Contract,
			Steps:     cand.Steps,
		}
		trial := p
		trial.meta = map[string]string{}
		maps.Copy(trial.meta, p.meta)
		maps.Copy(trial.meta, learned)
		vrec, verr := runEffective(sk.Name, verify, trial, dir, log)
		if verr != nil {
			lastErr = verr
			continue
		}
		if !vrec.Attest.Valid {
			storeAttest(cfg, vrec, log)
			lastErr = fmt.Errorf("candidate failed: %v / effects %v", vrec.Attest.Failed, vrec.Attest.Effects)
			continue
		}
		// Verified fix: fold it in under this coordinate's guard and save.
		folded, ferr := dhntskills.FoldForContext(effective, p.probes, cand.Steps)
		if ferr != nil {
			lastErr = ferr
			continue
		}
		if vs != nil {
			fcanon, err := dhntskills.LineariseDhnt(folded)
			if err != nil {
				lastErr = err
				continue
			}
			fid, _ := dhntskills.Identity(folded)
			if err := vs.Save(dhntskills.DerivedVersion{
				ParentID: p.rootID, ID: fid, Canonical: fcanon,
				ContextKey: p.ctxKey, Attest: vrec.Attest,
			}); err != nil {
				fmt.Fprintf(log, "skills: overlay save: %v\n", err)
			} else {
				fmt.Fprintf(log, "skills: %s: fix folded into host overlay (%s)\n", sk.Name, shortID(fid))
			}
		}
		if err := saveBindings(cfg.cfgDir, sk.Name, learned); err != nil {
			fmt.Fprintf(log, "skills: bindings save: %v\n", err)
		}
		storeAttest(cfg, vrec, log)
		return vrec, OutcomeRepaired, nil
	}
	return rec, OutcomeFailed, fmt.Errorf("skills: %q repair failed after %d attempts: %v", sk.Name, attempts, lastErr)
}
