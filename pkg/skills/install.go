package skills

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Admission is the verified-admission report for one skill: the same
// structural gate serves `add` (before installing) and `verify` (on an
// installed/embedded skill). It is deliberately structural in P1 —
// contract predicates are validated and reported, but evaluating them
// against the world needs the executor bindings that land with run.
type Admission struct {
	Name       string   `json:"name"`
	Ring       string   `json:"ring,omitempty"`
	Valid      bool     `json:"valid"` // structural gate: frontmatter + requires + canonical face
	Problems   []string `json:"problems,omitempty"`
	Applicable bool     `json:"applicable"` // requires hold at this coordinate
	Failing    string   `json:"failing,omitempty"`
	Unchecked  string   `json:"unchecked_compat,omitempty"`
	Identity   string   `json:"identity,omitempty"` // dhnt content address (dual bundle only)
	Contract   []string `json:"contract,omitempty"`
	EffectCap  []string `json:"effect_cap,omitempty"`
	ContextKey string   `json:"context_key,omitempty"` // over KeyProbes(skill)
}

// admit runs the structural gate + applicability check on a parsed skill.
func admit(sk Skill, ps *ProbeSet) Admission {
	a := Admission{Name: sk.Name, Ring: sk.Ring.String(), Valid: true}
	if sk.Name == "" {
		a.Valid = false
		a.Problems = append(a.Problems, "frontmatter: name missing")
	}
	if sk.Description == "" {
		a.Valid = false
		a.Problems = append(a.Problems, "frontmatter: description missing")
	}
	if sk.RequiresErr != "" {
		a.Valid = false
		a.Problems = append(a.Problems, sk.RequiresErr)
	}
	if sk.Dhnt != nil {
		if sk.Dhnt.Valid() {
			a.Identity = sk.Dhnt.Identity
			a.Contract = sk.Dhnt.Contract
			a.EffectCap = sk.Dhnt.EffectCap
		} else {
			a.Valid = false
			a.Problems = append(a.Problems, "skill.dhnt: "+sk.Dhnt.Err)
		}
	}
	v := verdictOf(sk, ps)
	a.Applicable = v.Applicable
	a.Failing = v.Failing
	a.Unchecked = v.Unchecked
	a.ContextKey = ContextKey(ps.Snapshot(KeyProbes(sk)))
	return a
}

// loadSkillDir parses a skill folder on disk (the `add` source).
func loadSkillDir(dir string) (Skill, error) {
	body, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return Skill{}, fmt.Errorf("skills: %s: no SKILL.md (%w)", dir, err)
	}
	sk, err := ParseFrontmatter(body)
	if err != nil {
		return Skill{}, err
	}
	if sk.Name == "" {
		sk.Name = filepath.Base(filepath.Clean(dir))
	}
	sk.Ring = RingLocal
	sk.Dir = dir
	if canon, err := os.ReadFile(filepath.Join(dir, "skill.dhnt")); err == nil {
		sk.HasDhnt = true
		sk.Dhnt = parseDhntInfo(canon)
	}
	return sk, nil
}

// installSkill copies an admitted skill folder into the ring-1 store.
// The admission gate must have passed before calling this.
func installSkill(srcDir, storeDir, name string, force bool) (string, error) {
	dst := filepath.Join(storeDir, name)
	if _, err := os.Stat(dst); err == nil {
		if !force {
			return "", fmt.Errorf("skills: %q already installed (use --force to replace)", name)
		}
		if err := os.RemoveAll(dst); err != nil {
			return "", err
		}
	}
	if err := copyDir(srcDir, dst); err != nil {
		_ = os.RemoveAll(dst) // never leave a half-installed skill
		return "", err
	}
	return dst, nil
}

// copyDir copies a skill folder: regular files and directories only —
// symlinks, devices, and anything irregular are skipped (a skill bundle
// is plain content; links could escape the store).
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// renderAdmission is the token-lean text form shared by add and verify.
func renderAdmission(w io.Writer, a Admission) {
	fmt.Fprintf(w, "skill: %s\n", a.Name)
	if a.Ring != "" {
		fmt.Fprintf(w, "ring: %s\n", a.Ring)
	}
	fmt.Fprintf(w, "valid: %v\n", a.Valid)
	for _, p := range a.Problems {
		fmt.Fprintf(w, "problem: %s\n", p)
	}
	if a.Applicable {
		fmt.Fprintln(w, "applicable: true")
	} else {
		fmt.Fprintf(w, "applicable: false (%s)\n", a.Failing)
	}
	if a.Unchecked != "" {
		fmt.Fprintf(w, "unchecked-compat: %s\n", a.Unchecked)
	}
	if a.Identity != "" {
		fmt.Fprintf(w, "identity: %s\n", a.Identity)
		fmt.Fprintf(w, "contract: %s\n", strings.Join(a.Contract, " "))
		fmt.Fprintf(w, "effect-cap: %s\n", strings.Join(a.EffectCap, " "))
	}
	fmt.Fprintf(w, "context: %s\n", a.ContextKey)
}
