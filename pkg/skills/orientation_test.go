package skills

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/unit"
)

// One contract, stated in both orientations: a dhnt face (cap {read write},
// contract {gereeni}) and a Bash# body whose decorators say the same.
const (
	orientDhnt = "sokilili demo efefecato reada wurite fini sotepo wana reada fini enisure gereeni fini fini\n"
	orientBody = "gereeni() { true; }\n\n@effects(\"read,write\")\n@ensure(\"gereeni\")\nreada() { true; }\n\nreada\n"
	// The same promise in keyword form, clauses reordered: still one capability.
	orientKeyword = "gereeni() { true; }\n\n@ensure(\"gereeni\")\n@guard(effects: \"write, read\")\nreada() { true; }\n\nreada\n"
	// Effects but no Require/Ensure: a cap, not a contract.
	orientBare = "@effects(\"read\")\nreada() { true; }\n\nreada\n"
)

func orientDoc(name, meta, body string) string {
	doc := "---\nname: " + name + "\ndescription: orientation fixture\n"
	if meta != "" {
		doc += "metadata:\n  " + meta + "\n"
	}
	doc += "---\n# " + name + "\n"
	if body != "" {
		doc += "\n```bsh\n" + body + "```\n"
	}
	return doc
}

func orientConfig(t *testing.T, fs fstest.MapFS, runner BashSharpRunner) *config {
	t.Helper()
	return &config{cfgDir: t.TempDir(), statics: map[string]string{"bashy": "test"}, bashSharpRunner: runner,
		sources: []Source{EmbedSource(fs, RingEmbedded)}}
}

func TestOrientation(t *testing.T) {
	bodyID := unit.BodyIdentity([]byte(orientBody))
	fs := fstest.MapFS{
		"by-dhnt/SKILL.md":     {Data: []byte(orientDoc("by-dhnt", "", ""))},
		"by-dhnt/skill.dhnt":   {Data: []byte(orientDhnt)},
		"by-bsh/SKILL.md":      {Data: []byte(orientDoc("by-bsh", "", orientBody))},
		"by-kw/SKILL.md":       {Data: []byte(orientDoc("by-kw", "", orientKeyword))},
		"derived/SKILL.md":     {Data: []byte(orientDoc("derived", DerivedFromKey+": "+bodyID, orientBody))},
		"derived/skill.dhnt":   {Data: []byte(orientDhnt)},
		"ambiguous/SKILL.md":   {Data: []byte(orientDoc("ambiguous", "", orientBody))},
		"ambiguous/skill.dhnt": {Data: []byte(orientDhnt)},
		// A derivation recorded against the body before a one-byte edit.
		"stale/SKILL.md":   {Data: []byte(orientDoc("stale", DerivedFromKey+": "+bodyID, strings.Replace(orientBody, "true", "true ", 1)))},
		"stale/skill.dhnt": {Data: []byte(orientDhnt)},
		"bare/SKILL.md":    {Data: []byte(orientDoc("bare", "", orientBare))},
	}
	ran := 0
	cfg := orientConfig(t, fs, func(context.Context, string, []string, string, io.Writer, io.Writer) (int, error) {
		ran++
		return 0, nil
	})
	get := func(name string) (Skill, Source) {
		t.Helper()
		sk, src, ok := cfg.catalog().Get(name)
		if !ok {
			t.Fatalf("missing fixture %q", name)
		}
		return sk, src
	}

	t.Run("paired faces share one capability key", func(t *testing.T) {
		d, _ := get("by-dhnt")
		b, _ := get("by-bsh")
		k, _ := get("by-kw")
		if d.Unit.Face != FaceDhnt || b.Unit.Face != FaceBashSharp {
			t.Fatalf("faces: %q %q", d.Unit.Face, b.Unit.Face)
		}
		if !strings.HasPrefix(d.Unit.Capability, "k") || d.Unit.Capability != b.Unit.Capability || b.Unit.Capability != k.Unit.Capability {
			t.Fatalf("capability keys differ across orientations: dhnt=%q bsh=%q keyword=%q", d.Unit.Capability, b.Unit.Capability, k.Unit.Capability)
		}
		if !d.Unit.Verified() || !b.Unit.Verified() {
			t.Fatalf("contracted units not verified: %+v %+v", d.Unit, b.Unit)
		}
		der, _ := get("derived")
		if !der.Unit.Valid() || !der.Unit.Derived || der.Unit.Identity != bodyID || der.Unit.Capability != b.Unit.Capability {
			t.Fatalf("declared derivation: %+v", der.Unit)
		}
	})

	t.Run("one source byte changes the identity", func(t *testing.T) {
		b, _ := get("by-bsh")
		if b.Unit.Identity != bodyID {
			t.Fatalf("identity %q, want body identity %q", b.Unit.Identity, bodyID)
		}
		edited := strings.Replace(orientBody, "true", "true ", 1)
		u := ResolveUnit([]byte(orientDoc("by-bsh", "", edited)), nil, nil)
		if u.Identity == bodyID || !strings.HasPrefix(u.Identity, "h") {
			t.Fatalf("a one-byte edit kept identity %q", u.Identity)
		}
		if u.Capability != b.Unit.Capability {
			t.Fatalf("a step edit moved the capability: %q -> %q", b.Unit.Capability, u.Capability)
		}
		// CRLF is not a byte of the unit.
		crlf := strings.ReplaceAll(orientDoc("by-bsh", "", orientBody), "\n", "\r\n")
		if got := ResolveUnit([]byte(crlf), nil, nil).Identity; got != bodyID {
			t.Fatalf("CRLF moved identity: %q", got)
		}
	})

	t.Run("ambiguous unit is refused", func(t *testing.T) {
		ps, _ := cfg.probes(false)
		for _, name := range []string{"ambiguous", "stale"} {
			sk, src := get(name)
			if sk.Unit.Valid() || !strings.Contains(sk.Unit.Err, "ambiguous unit") {
				t.Fatalf("%s: not refused: %+v", name, sk.Unit)
			}
			if _, err := prepareRun(cfg, sk, src, ps); err == nil || !strings.Contains(err.Error(), "ambiguous unit") {
				t.Fatalf("%s: run not refused: %v", name, err)
			}
			if a := admit(sk, ps); a.Valid || a.Identity != "" {
				t.Fatalf("%s: admitted: %+v", name, a)
			}
		}
		if ran != 0 {
			t.Fatalf("an ambiguous unit reached the runner %d time(s)", ran)
		}
		// The same refusal at the install gate, from a folder on disk.
		dir := filepath.Join(t.TempDir(), "ambiguous")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "SKILL.md"), fs["ambiguous/SKILL.md"].Data, 0o644)
		os.WriteFile(filepath.Join(dir, "skill.dhnt"), []byte(orientDhnt), 0o644)
		sk, err := loadSkillDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if a := admit(sk, ps); a.Valid {
			t.Fatalf("install gate admitted an ambiguous unit: %+v", a)
		}
	})

	t.Run("contract-less is never verified", func(t *testing.T) {
		sk, src := get("bare")
		if !sk.Unit.Valid() || sk.Unit.Verified() || sk.Unit.Capability != "" {
			t.Fatalf("bare unit: %+v", sk.Unit)
		}
		ps, _ := cfg.probes(false)
		var log strings.Builder
		rec, _, err := runSkill(cfg, sk, src, ps, t.TempDir(), &log)
		if err != nil || rec.Valid || rec.Attest.Valid || !rec.Uncontracted || rec.Capability != "" || !ranClean(rec) {
			t.Fatalf("uncontracted run: %+v %v", rec, err)
		}
		if _, err := os.Stat(filepath.Join(cfg.cfgDir, "attest", "bare.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("an uncontracted run left a receipt: %v", err)
		}
		f := &cobraRunner{t: t, opts: []Option{WithConfigDir(cfg.cfgDir), WithSource(src), WithBashSharpRunner(cfg.bashSharpRunner)}}
		stdout, _, err := f.run("run", "bare", "--json")
		if err != nil || !strings.Contains(stdout, `"uncontracted":true`) || strings.Contains(stdout, `"valid":true`) {
			t.Fatalf("CLI: %s %v", stdout, err)
		}
		// And a contracted Bash# receipt carries both halves.
		b, bsrc := get("by-bsh")
		rec, _, err = runSkill(cfg, b, bsrc, ps, t.TempDir(), &log)
		if err != nil || !rec.Valid || rec.Identity != bodyID || rec.Capability != b.Unit.Capability || rec.Uncontracted {
			t.Fatalf("contracted receipt: %+v %v", rec, err)
		}
	})
}
