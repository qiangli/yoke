package verify

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/shape/evidence"
	"github.com/qiangli/yoke/pkg/unit/frontmatter"
	"gopkg.in/yaml.v3"
)

// CompatCheck is the name of the built-in compatibility check in a record.
const CompatCheck = "shape/compat"

// Surface kinds. A surface element is "<kind>:<name>".
const (
	KindUnit     = "unit"
	KindRoute    = "route"
	KindFlag     = "flag"
	KindArg      = "argument"
	KindProperty = "property"
)

// Surface is the declared data shape and agreed interface of one tree: unit
// names, routes, the flags and arguments units declare in their
// frontmatter (`flags:` / `args:` lists), and the stored properties listed
// in shaped.json (`"properties": [...]`). Only declarations are read; stored
// data itself is never inspected.
type Surface map[string]bool

// ReadSurface reads the declared surface of the tree rooted at dir. It reads
// the same way on every OS: slash-separated names, no inference.
func ReadSurface(dir string) (Surface, error) {
	s := Surface{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", ".shape":
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasPrefix(rel, "public/"):
			s[KindRoute+":/"+strings.TrimPrefix(rel, "public/")] = true
		case strings.HasPrefix(rel, "api/") && strings.HasSuffix(rel, ".md"):
			name := strings.TrimSuffix(strings.TrimPrefix(rel, "api/"), ".md")
			s[KindUnit+":"+name] = true
			s[KindRoute+":/api/"+name] = true
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			if b, perr := frontmatter.Parse(data); perr == nil {
				for _, f := range names(b.Top["flags"]) {
					s[KindFlag+":"+name+" "+f] = true
				}
				for _, a := range append(names(b.Top["args"]), names(b.Top["arguments"])...) {
					s[KindArg+":"+name+" "+a] = true
				}
			}
		case rel == "shaped.json":
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			var m struct {
				Properties []string `json:"properties"`
			}
			if jerr := json.Unmarshal(data, &m); jerr != nil {
				return fmt.Errorf("shaped.json: %w", jerr)
			}
			for _, prop := range m.Properties {
				s[KindProperty+":"+prop] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("verify: read surface: %w", err)
	}
	return s, nil
}

func names(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.SequenceNode {
		var out []string
		for _, c := range n.Content {
			if v := strings.TrimSpace(c.Value); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	if v := strings.TrimSpace(n.Value); v != "" {
		return []string{v}
	}
	return nil
}

// Compat is the comparison of a candidate's surface against the base's.
type Compat struct {
	// Removed are base elements missing from the candidate and not
	// approved for dropping; Hints maps each to a probable rename target.
	Removed []string
	Hints   map[string]string
	// Added are candidate-only elements; adding is always fine.
	Added []string
	// Contracted are removals the user approved.
	Contracted []string
}

// Failed reports whether an unapproved removal or rename was found.
func (c Compat) Failed() bool { return len(c.Removed) > 0 }

// CompareSurface compares base to candidate. approved lists elements the
// user explicitly approved dropping (the separate contract step).
func CompareSurface(base, cand Surface, approved []string) Compat {
	c := Compat{Hints: map[string]string{}}
	var gone, added []string
	for k := range base {
		if !cand[k] {
			gone = append(gone, k)
		}
	}
	for k := range cand {
		if !base[k] {
			added = append(added, k)
		}
	}
	sort.Strings(gone)
	sort.Strings(added)
	c.Added = added
	for _, k := range gone {
		if slices.Contains(approved, k) {
			c.Contracted = append(c.Contracted, k)
			continue
		}
		c.Removed = append(c.Removed, k)
		kind, _, _ := strings.Cut(k, ":")
		for _, a := range added {
			if strings.HasPrefix(a, kind+":") {
				c.Hints[k] = a
				break
			}
		}
	}
	return c
}

// Detail renders the reason a comparison failed.
func (c Compat) Detail() string {
	if !c.Failed() {
		return ""
	}
	var b strings.Builder
	b.WriteString("removing or renaming declared data or interface elements breaks rollback to earlier versions. ")
	for i, k := range c.Removed {
		if i > 0 {
			b.WriteString("; ")
		}
		if h, ok := c.Hints[k]; ok {
			fmt.Fprintf(&b, "%s was renamed or replaced by %s", k, h)
		} else {
			fmt.Fprintf(&b, "%s was removed", k)
		}
	}
	b.WriteString(". Keep the old element and add the new one with a fallback for old data; dropping needs a separate user-approved contract step.")
	return b.String()
}

// Result renders the comparison as one check result.
func (c Compat) Result() evidence.CheckResult {
	if c.Failed() {
		return evidence.CheckResult{Name: CompatCheck, Status: evidence.StatusFail, Detail: c.Detail()}
	}
	d := fmt.Sprintf("%d added, %d approved removals", len(c.Added), len(c.Contracted))
	return evidence.CheckResult{Name: CompatCheck, Status: evidence.StatusPass, Detail: d}
}
