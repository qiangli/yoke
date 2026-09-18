package skills

// The `kind: skill` Record — a LOSSLESS projection of a skill folder for the
// catalog and the wire.
//
// SKILL.md stays the on-disk canonical and the authoring form; the record is
// never edited by hand. It exists so a skill can travel as ONE document (an
// asset-registry Content blob, a `skill get` payload) and come back as the
// same folder, byte for byte. Three consequences shape the type:
//
//   - `files` carries every file verbatim. That is what makes it lossless:
//     WriteFolder writes those bytes and nothing else, so folder → record →
//     folder is the identity function.
//   - identity, capability and contract are DERIVED — a view of DhntInfo over
//     the canonical face that is already inside `files`. They are computed at
//     RecordOf and ignored at import: a re-parse of the rebuilt folder
//     re-derives them, so a record cannot claim an identity its bytes do not
//     hash to. A skill without skill.dhnt has identity "" and capability "",
//     honestly, never invented.
//   - the emit is deterministic (fixed key order, sorted maps, no timestamps)
//     so a content hash over the bytes is stable across hosts and calls.
//
// A record is shareable by construction, which is why packing runs pkg/redact
// over every file and REFUSES on a hit rather than scrubbing: a partially
// redacted skill is a skill that no longer works, silently.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/redact"
)

// RecordKind is the only value Record.Kind takes.
const RecordKind = "skill"

// Record is the catalog/wire projection of one skill folder. Field order is
// the emitted key order and is fixed.
type Record struct {
	Name        string `yaml:"name" json:"name"`
	Kind        string `yaml:"kind" json:"kind"` // always RecordKind
	Description string `yaml:"description" json:"description"`
	// Requires is the frontmatter `metadata.requires` expression verbatim —
	// the spelling the author wrote, not the parsed form.
	Requires string `yaml:"requires,omitempty" json:"requires,omitempty"`
	// Identity and Capability are derived from skill.dhnt (see DhntInfo);
	// both are "" when the folder has no valid canonical face.
	Identity   string         `yaml:"identity" json:"identity"`
	Capability string         `yaml:"capability" json:"capability"`
	Contract   RecordContract `yaml:"contract" json:"contract"`
	// Bindings are the check-*/step-* frontmatter metadata keys that bind
	// contract predicates and step primitives to concrete commands.
	Bindings map[string]string `yaml:"bindings,omitempty" json:"bindings,omitempty"`
	// Files maps slash-separated relative paths to file contents verbatim,
	// SKILL.md included.
	Files map[string]string `yaml:"files" json:"files"`
}

// RecordContract is the record's view of DhntInfo: what the skill guarantees.
type RecordContract struct {
	Effects []string `yaml:"effects" json:"effects"` // declared effect-cap atoms
	Ensure  []string `yaml:"ensure" json:"ensure"`   // contract predicate ids
	Steps   int      `yaml:"steps" json:"steps"`
}

// ErrCarriesIdentity reports a file that names host identity, which a
// shareable record must not carry. The error names the path and the KINDS
// found, never the values — a diagnostic that quoted the hostname would be
// the leak it reports.
type ErrCarriesIdentity struct {
	Path  string
	Kinds []string
}

func (e *ErrCarriesIdentity) Error() string {
	return fmt.Sprintf("skills: %s carries host identity (%s) — a skill record is shareable, so it is refused rather than partially redacted",
		e.Path, strings.Join(e.Kinds, ", "))
}

// RecordOf packs a skill that lives on disk, walking its folder in sorted
// path order and scrubbing against this host's identity. A skill from the
// embedded ring has no folder; pack it with RecordFrom and its source.
func RecordOf(sk *Skill) (Record, error) {
	if sk == nil || sk.Dir == "" {
		return Record{}, errors.New("skills: record: skill has no folder on disk — use RecordFrom with its source")
	}
	return RecordFrom(sk, DirSource(filepath.Dir(sk.Dir)), redact.FromHost())
}

// RecordFrom packs a skill through the ring source that serves it, with an
// explicit scrubber (hermetic in tests; redact.FromHost in production).
// The entry name is the folder name, which is the key every source resolves
// by; it may differ from the frontmatter name the record advertises.
func RecordFrom(sk *Skill, src Source, scrub *redact.Scrubber) (Record, error) {
	if sk == nil {
		return Record{}, errors.New("skills: record: nil skill")
	}
	entry := sk.Name
	if sk.Dir != "" {
		entry = filepath.Base(sk.Dir)
	}
	paths, err := src.Files(entry)
	if err != nil {
		return Record{}, fmt.Errorf("skills: record %s: %w", entry, err)
	}
	sort.Strings(paths)
	files := make(map[string]string, len(paths))
	for _, rel := range paths {
		data, ok := src.File(entry, rel)
		if !ok {
			return Record{}, fmt.Errorf("skills: record %s: %s vanished while packing", entry, rel)
		}
		if scrub != nil {
			if _, found := scrub.Scrub(string(data)); len(found) > 0 {
				return Record{}, &ErrCarriesIdentity{Path: entry + "/" + rel, Kinds: findingKinds(found)}
			}
		}
		files[rel] = string(data)
	}
	if _, ok := files[skillMarker]; !ok {
		return Record{}, fmt.Errorf("skills: record %s: no %s", entry, skillMarker)
	}
	rec := Record{
		Name:        sk.Name,
		Kind:        RecordKind,
		Description: sk.Description,
		Requires:    sk.Meta["requires"],
		Files:       files,
	}
	// The same key rule the executor and the repair prompt apply (run.go
	// CheckBindingKey, adapt.go): `exito` with no name arg binds bare `check`.
	for k, v := range sk.Meta {
		if strings.HasPrefix(k, "step-") || strings.HasPrefix(k, "check") {
			if rec.Bindings == nil {
				rec.Bindings = map[string]string{}
			}
			rec.Bindings[k] = v
		}
	}
	if sk.Dhnt.Valid() {
		rec.Identity = sk.Dhnt.Identity
		rec.Capability = sk.Dhnt.Capability
		rec.Contract = RecordContract{
			Effects: append([]string(nil), sk.Dhnt.EffectCap...),
			Ensure:  append([]string(nil), sk.Dhnt.Contract...),
			Steps:   sk.Dhnt.Steps,
		}
	}
	return rec, nil
}

// ValidateRecordShareable applies the same fail-closed identity gate used by
// RecordFrom to a record received over the wire. Importers must call this
// before rebuilding the folder: stdin must not be a softer admission path.
func ValidateRecordShareable(rec Record, scrub *redact.Scrubber) error {
	if scrub == nil {
		return nil
	}
	paths := make([]string, 0, len(rec.Files))
	for path := range rec.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if _, found := scrub.Scrub(rec.Files[path]); len(found) > 0 {
			return &ErrCarriesIdentity{Path: rec.Name + "/" + path, Kinds: findingKinds(found)}
		}
	}
	return nil
}

// findingKinds lists the distinct kinds in a scrub result, sorted.
func findingKinds(found []redact.Finding) []string {
	seen := map[redact.Kind]bool{}
	var kinds []string
	for _, f := range found {
		if !seen[f.Kind] {
			seen[f.Kind] = true
			kinds = append(kinds, string(f.Kind))
		}
	}
	sort.Strings(kinds)
	return kinds
}

// MarshalRecord emits a record as canonical YAML: struct fields in declared
// order, map keys sorted (yaml.v3 does both), nothing time-dependent. Two
// calls over equal records yield equal bytes, whatever order the maps were
// built in, so a timestamp-free content hash over the output is stable.
func MarshalRecord(rec Record) ([]byte, error) {
	if rec.Kind != RecordKind {
		return nil, fmt.Errorf("skills: record kind %q, want %q", rec.Kind, RecordKind)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ParseRecord decodes a record strictly: an unknown top-level key is refused
// (a typo'd field would otherwise vanish silently), and kind must be "skill".
// The derived fields are decoded as written but carry no authority — see
// WriteFolder.
func ParseRecord(b []byte) (Record, error) {
	var rec Record
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&rec); err != nil {
		return Record{}, fmt.Errorf("skills: record: %w", err)
	}
	if rec.Kind != RecordKind {
		return Record{}, fmt.Errorf("skills: record kind %q, want %q", rec.Kind, RecordKind)
	}
	if rec.Name == "" {
		return Record{}, errors.New("skills: record has no name")
	}
	if _, ok := rec.Files[skillMarker]; !ok {
		return Record{}, fmt.Errorf("skills: record %s has no %s", rec.Name, skillMarker)
	}
	return rec, nil
}

// WriteFolder rebuilds the skill folder from rec.Files under dir, byte for
// byte. Only files are written: identity, capability and contract are
// re-derived by whoever next parses the folder, so an imported record cannot
// assert an identity its bytes do not have.
//
// dir must be absent or an empty directory — the caller decides whether an
// existing folder is replaced, and removes it first; writing over a
// populated folder could leave files the record never mentioned. Every path
// must stay inside dir.
func WriteFolder(rec Record, dir string) error {
	if rec.Kind != RecordKind {
		return fmt.Errorf("skills: record kind %q, want %q", rec.Kind, RecordKind)
	}
	if _, ok := rec.Files[skillMarker]; !ok {
		return fmt.Errorf("skills: record %s has no %s", rec.Name, skillMarker)
	}
	paths := make([]string, 0, len(rec.Files))
	for rel := range rec.Files {
		if rel == "" || strings.HasPrefix(rel, "/") || !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("skills: record %s: path %q escapes the skill folder", rec.Name, rel)
		}
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("skills: %s exists and is not empty — remove it to replace", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, rel := range paths {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(rec.Files[rel]), 0o644); err != nil {
			return err
		}
	}
	return nil
}
