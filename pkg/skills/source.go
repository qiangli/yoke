package skills

import (
	"io/fs"
	"path/filepath"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/spacetime"
)

// The ring machinery (Ring, Source, the merge order, shadowing) lives in
// pkg/assetring so skills, tools, models, and agents all resolve names
// the same way. A skill is a folder identified by its SKILL.md.

const skillMarker = "SKILL.md"

// Ring names where a skill came from. Later sources shadow earlier ones
// on a name collision (a host-local override of a shared or embedded
// skill is deliberate, and reported).
type Ring = assetring.Ring

const (
	RingEmbedded = assetring.RingEmbedded // compiled into the host binary
	RingShared   = assetring.RingShared   // a shared catalog dir — read-only
	RingLocal    = assetring.RingLocal    // host-local store (installed + learned)
)

// Source is one ring's skill supply.
type Source = assetring.Source

// EmbedSource wraps an fs.FS whose top-level directories are skills
// (the shape of bashy's embedded skills FS).
func EmbedSource(fsys fs.FS, ring Ring) Source {
	return assetring.FolderFS(fsys, ring, skillMarker)
}

// DirSource serves the host-local ring from a directory of skill
// folders. A missing directory is an empty source, not an error.
func DirSource(dir string) Source {
	return assetring.FolderDir(dir, RingLocal, skillMarker)
}

// SharedDirSource serves a shared catalog directory — any git clone or
// synced folder of skill folders — as a read-only ring between embedded
// and local. This is the standalone sharing path ("cloud as a thin
// replaceable relay"): a team's skills repo cloned to disk IS a catalog;
// no control plane required. Local installs/learning still shadow it.
func SharedDirSource(dir string) Source {
	return assetring.FolderDir(dir, RingShared, skillMarker)
}

// ExitCode maps a cobra error to the repo exit convention (2 usage, 1 otherwise).
func ExitCode(err error) int { return assetring.ExitCode(err) }

// Listing is one catalog row: the skill plus its applicability verdict
// at this host's coordinate.
type Listing struct {
	Skill
	Verdict Verdict
	Shadows bool // this row hides a same-named skill in a lower ring
	Warning string
}

// Catalog merges sources; on a name collision the LAST source wins
// (ring order: embedded first, local last — local shadows embedded).
type Catalog struct{ Sources []Source }

func (c *Catalog) ring() *assetring.Catalog[Skill] {
	return &assetring.Catalog[Skill]{Sources: c.Sources, Parse: parseSkill}
}

// Get returns a skill's parsed entry and its winning source.
func (c *Catalog) Get(name string) (Skill, Source, bool) { return c.ring().Get(name) }

// List returns every row with verdicts attached. Filtering is the
// caller's business — `--all`, JSON views, and future consumers choose
// their own cut.
func (c *Catalog) List(ps *ProbeSet) ([]Listing, error) {
	rows, err := c.ring().Rows()
	if err != nil {
		return nil, err
	}
	out := make([]Listing, 0, len(rows))
	for _, r := range rows {
		l := Listing{Skill: r.Entry, Shadows: r.Shadows}
		l.Verdict = verdictOf(l.Skill, ps)
		if l.RequiresErr != "" {
			l.Warning = "requires unparsable: " + l.RequiresErr
		}
		out = append(out, l)
	}
	return out, nil
}

func parseSkill(dirName string, body []byte, src Source) Skill {
	sk, err := ParseFrontmatter(body)
	if err != nil {
		// Degrade, never hide: directory name, no description, rung-0.
		sk = Skill{RequiresErr: err.Error(), Meta: map[string]string{}}
	}
	if sk.Name == "" {
		sk.Name = dirName
	}
	sk.Ring = src.Ring()
	if ds, ok := src.(assetring.DirSourcer); ok {
		sk.Dir = filepath.Join(ds.Dir(), dirName)
	}
	if canon, ok := src.File(dirName, "skill.dhnt"); ok {
		sk.HasDhnt = true
		sk.Dhnt = parseDhntInfo(canon)
	}
	_, sk.HasTasks = src.File(dirName, "tasks.md")
	if !sk.HasTasks && sk.Meta["tasks"] != "" {
		sk.HasTasks = true // tasks POINTER (resolved under cwd at run time)
	}
	return sk
}

// verdictOf applies the pinned gating table: structured requires gate
// exactly; compatibility free text alone flags but never filters; no
// info at all is applicable (rung-0 permissive).
func verdictOf(sk Skill, ps *ProbeSet) Verdict {
	if sk.Requires != nil {
		return sk.Requires.Eval(ps)
	}
	v := Verdict{Applicable: true}
	if sk.Compatibility != "" || sk.RequiresErr != "" {
		v.Unchecked = sk.Compatibility
		if v.Unchecked == "" {
			v.Unchecked = "requires unparsable"
		}
	}
	return v
}

// KeyProbes returns the probe names a skill's context key is computed
// over: {os, arch} ∪ the requires-referenced subset (pinned rule),
// minus any probe that must not be keyed on.
//
// Two pins meet here, and both are load-bearing:
//
// The subset rule (§11.5) keys on the probes a skill actually references
// rather than the whole host snapshot. Keying on everything would make
// every host unique, so no two machines could ever share a coordinate
// and no overlay would ever be reused.
//
// The keyability rule is the same failure along the time axis instead of
// the space axis. A skill may legitimately GATE on `time.attended`; it
// may never be KEYED by it, because a coordinate that moves with the
// clock files every run under a fresh address and the evidence corpus
// fragments to n=1 — silently, since nothing is broken, only useless.
// Network locality is deliberately NOT filtered: reachability is a real
// capability difference, so an entry referencing it should re-key on a
// roam. See spacetime.IsUnkeyableProbe for where that line falls.
func KeyProbes(sk Skill) []string {
	names := []string{"os", "arch"}
	if sk.Requires != nil {
		for _, p := range sk.Requires.ProbeRefs() {
			if p != "os" && p != "arch" {
				names = append(names, p)
			}
		}
	}
	return spacetime.KeyableProbes(names)
}
