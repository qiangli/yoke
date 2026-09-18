// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package release

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Target is one (build, goos, goarch) cell of the matrix.
type Target struct {
	BuildID string
	Goos    string
	Goarch  string
	// Binary is the binary name inside the archive (with .exe on windows).
	Binary string
	// Path is where the compiled binary lands under dist/.
	Path string
	Main string
	Dir  string
	Env  []string
	// Flags and Ldflags are passed through to the builder verbatim.
	Flags []string
	// Tags become a single -tags argument, comma-joined.
	Tags    []string
	Ldflags []string
}

// ArchivePlan is one archive to write.
type ArchivePlan struct {
	ArchiveID string
	Target    Target
	Format    string
	// Name is the rendered archive file name, extension included.
	Name string
	Path string
	// Members are the files that go into it.
	Members []Member
}

// Plan is the whole resolved release: what to build, what to package, and
// what the checksum manifest is called.
type Plan struct {
	ProjectName  string
	Version      string
	Tag          string
	Dist         string
	Targets      []Target
	Archives     []ArchivePlan
	ChecksumName string
}

// BuildPlan resolves cfg against fields into a concrete Plan. It renders
// every name template up front, so a template error fails BEFORE anything is
// compiled rather than after a ten-minute matrix build.
func BuildPlan(cfg *Config, fields Fields) (*Plan, error) {
	p := &Plan{
		ProjectName: cfg.ProjectName,
		Version:     fields.Version,
		Tag:         fields.Tag,
		Dist:        cfg.Dist,
	}
	for _, b := range cfg.Builds {
		if b.Skip {
			continue
		}
		cells, err := matrix(b)
		if err != nil {
			return nil, err
		}
		for _, t := range cells {
			bin := b.Binary
			if t.Goos == "windows" {
				bin += ".exe"
			}
			if err := checkMemberName(bin); err != nil {
				return nil, fmt.Errorf("builds[%s].binary: %w", b.ID, err)
			}
			// Ldflags carry the version stamp, so they are rendered against the
			// SAME fields the names are — with this target's Os/Arch bound.
			//
			// Leaving them literal is not a cosmetic miss. `-X main.version={{.Version}}`
			// (no spaces) is accepted by the linker verbatim, and the released binary
			// then REPORTS "{{.Version}}" as its version — a green release that lies
			// about what it is. The spaced spelling fails loudly instead, which is how
			// this was found; both are the same defect.
			tf := fields
			tf.Os, tf.Arch, tf.Binary = t.Goos, t.Goarch, bin
			ldflags, err := renderAll(b.Ldflags, tf)
			if err != nil {
				return nil, fmt.Errorf("builds[%s].ldflags: %w", b.ID, err)
			}
			flags, err := renderAll(b.Flags, tf)
			if err != nil {
				return nil, fmt.Errorf("builds[%s].flags: %w", b.ID, err)
			}
			env, err := renderAll(b.Env, tf)
			if err != nil {
				return nil, fmt.Errorf("builds[%s].env: %w", b.ID, err)
			}
			tgt := Target{
				BuildID: b.ID,
				Goos:    t.Goos,
				Goarch:  t.Goarch,
				Binary:  bin,
				Path:    filepath.Join(cfg.Dist, fmt.Sprintf("%s_%s_%s", b.ID, t.Goos, t.Goarch), bin),
				Main:    b.Main,
				Dir:     b.Dir,
				Env:     env,
				Flags:   flags,
				Tags:    b.Tags,
				Ldflags: ldflags,
			}
			p.Targets = append(p.Targets, tgt)
		}
	}
	if len(p.Targets) == 0 {
		return nil, fmt.Errorf("release: build matrix is empty")
	}

	for _, a := range cfg.Archives {
		ids := a.IDs
		for _, tgt := range p.Targets {
			if len(ids) > 0 && !slices.Contains(ids, tgt.BuildID) {
				continue
			}
			for _, format := range formatsFor(a, tgt.Goos) {
				f := fields
				f.Os, f.Arch, f.Binary = tgt.Goos, tgt.Goarch, tgt.Binary
				base, err := Render(a.NameTemplate, f)
				if err != nil {
					return nil, err
				}
				name := base
				if format != "binary" {
					name = base + "." + format
				}
				// The rendered name becomes a path under dist/. A template
				// resolving to "../x" or "a/b" would write outside the output
				// directory; a release must not be able to reach above its own
				// dist/.
				if err := checkArtifactName("archives["+a.ID+"].name_template", name); err != nil {
					return nil, err
				}
				members := []Member{{Name: tgt.Binary, Source: tgt.Path, Mode: 0o755}}
				if a.WrapInDirectory {
					members[0].Name = base + "/" + tgt.Binary
				}
				for _, extra := range a.Files {
					members = append(members, Member{Name: filepath.Base(extra), Source: extra, Mode: 0o644})
				}
				if err := checkMembers(a.ID, members); err != nil {
					return nil, err
				}
				p.Archives = append(p.Archives, ArchivePlan{
					ArchiveID: a.ID,
					Target:    tgt,
					Format:    format,
					Name:      name,
					Path:      filepath.Join(cfg.Dist, name),
					Members:   members,
				})
			}
		}
	}
	if len(p.Archives) == 0 {
		return nil, fmt.Errorf("release: no archive matched any build (check archives[].ids)")
	}

	if !cfg.Checksum.Disable {
		// Rendered with no target bound: Os/Arch/Binary are unset, so a
		// per-target template here is an error rather than "ycode--".
		name, err := Render(cfg.Checksum.NameTemplate, fields)
		if err != nil {
			return nil, err
		}
		if err := checkArtifactName("checksum.name_template", name); err != nil {
			return nil, err
		}
		p.ChecksumName = name
	}
	if err := checkDuplicateNames(p.Archives, p.ChecksumName); err != nil {
		return nil, err
	}
	return p, nil
}

type cell struct{ Goos, Goarch string }

func matrix(b Build) ([]cell, error) {
	var out []cell
	if len(b.Targets) > 0 {
		for _, t := range b.Targets {
			parts := strings.Split(t, "_")
			// A malformed target must not be skipped: silently dropping it
			// shrinks the matrix, and the release then ships fewer platforms
			// than the config declares with nothing reporting the gap.
			if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("release: builds[%s].targets: %q is not a goos_goarch target", b.ID, t)
			}
			out = append(out, cell{parts[0], parts[1]})
		}
		return out, nil
	}
	for _, os := range b.Goos {
		for _, arch := range b.Goarch {
			if ignored(b.Ignore, os, arch) {
				continue
			}
			out = append(out, cell{os, arch})
		}
	}
	return out, nil
}

// checkArtifactName refuses a rendered name that is not a plain file name.
// Artifact names are joined onto dist/, so a separator or a parent reference
// escapes the output directory. Both separators are rejected on every host: a
// config authored on unix must fail the same way when the release runs on
// Windows.
func checkArtifactName(what, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("release: %s rendered an empty name", what)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("release: %s rendered %q — artifact names must be plain file names inside dist/, not paths", what, name)
	case name == "." || name == "..":
		return fmt.Errorf("release: %s rendered %q, which is not a file name", what, name)
	}
	return nil
}

// checkMemberName refuses an in-archive path that would escape the extraction
// directory (the tar-slip shape) or that is not a name at all.
func checkMemberName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("release: empty archive member name")
	case strings.ContainsRune(name, '\\'):
		return fmt.Errorf("release: archive member %q contains a backslash — members are recorded with %q separators", name, "/")
	case path.IsAbs(name):
		return fmt.Errorf("release: archive member %q is an absolute path", name)
	}
	if slices.Contains(strings.Split(name, "/"), "..") {
		return fmt.Errorf("release: archive member %q escapes the archive root", name)
	}
	return nil
}

// checkMembers validates every member and refuses two members that would be
// written under one name — the second copy would shadow the first, so the
// archive would quietly hold different content than the config listed.
func checkMembers(archiveID string, members []Member) error {
	seen := map[string]string{}
	for _, m := range members {
		if err := checkMemberName(m.Name); err != nil {
			return fmt.Errorf("archives[%s]: %w", archiveID, err)
		}
		if prev, ok := seen[m.Name]; ok {
			return fmt.Errorf("release: archives[%s] packs two files as %q (%s and %s) — archives[].files entries must have distinct base names", archiveID, m.Name, prev, m.Source)
		}
		seen[m.Name] = m.Source
	}
	return nil
}

func ignored(list []Ignore, goos, goarch string) bool {
	for _, ig := range list {
		if (ig.Goos == "" || ig.Goos == goos) && (ig.Goarch == "" || ig.Goarch == goarch) {
			return true
		}
	}
	return false
}

func formatsFor(a Archive, goos string) []string {
	for _, o := range a.FormatOverrides {
		if o.Goos == goos && len(o.Formats) > 0 {
			return o.Formats
		}
	}
	return a.Formats
}

// checkDuplicateNames refuses a plan where two archives would write the same
// file. Left unchecked, the second silently overwrites the first and the
// release ships one platform's bytes under two platforms' names — the same
// failure mode that stranded the non-darwin fleet.
//
// checksumName is included because the manifest is written into the same dist/
// AFTER the archives are hashed: a collision there would overwrite a shipped
// archive with the checksum file while the ledger still carried the archive's
// digest, leaving a recorded hash that matches nothing on disk.
func checkDuplicateNames(archives []ArchivePlan, checksumName string) error {
	seen := map[string]string{}
	var dups []string
	for _, a := range archives {
		key := a.Name
		owner := a.Target.Goos + "/" + a.Target.Goarch
		if prev, ok := seen[key]; ok {
			dups = append(dups, fmt.Sprintf("%s (%s and %s)", key, prev, owner))
			continue
		}
		seen[key] = owner
	}
	if checksumName != "" {
		if prev, ok := seen[checksumName]; ok {
			dups = append(dups, fmt.Sprintf("%s (%s and the checksum manifest)", checksumName, prev))
		}
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		return fmt.Errorf("release: archive name collision: %s — the name_template must distinguish os/arch", strings.Join(dups, ", "))
	}
	return nil
}

// renderAll renders each entry of a config list against fields. A nil list
// stays nil so a caller cannot tell a rendered empty list from an absent one.
func renderAll(in []string, f Fields) ([]string, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		v, err := Render(s, f)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
