package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/shape/verify/repair"
	yunit "github.com/qiangli/yoke/pkg/unit"
)

// UnitReader is the contract reader backed by the A1 pkg/unit contract
// reader: each unit is parsed through the normative Bash# grammar, so all
// six decorators (@require @ensure @guard @effects @confirm @timeout, both
// the positional and the keyword spelling) count, and only where the shell
// would apply them. A file where pkg/unit finds no contract — unparseable,
// undecorated, or declaring effects without a checkable guarantee — falls
// back to the minimal line-based reader, so the verifier keeps answering
// everywhere it used to.
//
// A unit is a .bsh/.bash file or a Markdown document with Bash# fences;
// files under checks/ are required checks, as with the minimal reader.
type UnitReader struct {
	// Exts are the file extensions read as raw Bash# bodies.
	// Empty means .bsh and .bash.
	Exts []string
	// FenceExts are the extensions read as fenced documents. Empty means
	// .md.
	FenceExts []string
}

// ReadContract walks dir and returns its contract.
func (r UnitReader) ReadContract(dir string) (Contract, error) {
	exts := r.Exts
	if len(exts) == 0 {
		exts = []string{".bsh", ".bash"}
	}
	fences := r.FenceExts
	if len(fences) == 0 {
		fences = []string{".md"}
	}
	var c Contract
	effects := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		raw, fenced := slices.Contains(exts, ext), slices.Contains(fences, ext)
		if !raw && !fenced {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		body := string(data)
		if fenced {
			body = yunit.BashSharpBody(body)
			if strings.TrimSpace(body) == "" {
				return nil
			}
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, checksRoot) {
			sum := sha256.Sum256([]byte(body))
			c.Checks = append(c.Checks, Check{
				Name: rel, Body: body, BodyDigest: "sha256:" + hex.EncodeToString(sum[:]),
			})
			return nil
		}
		if funcs, uc, ok := repair.BodyFunctions(body, rel); ok {
			for _, f := range funcs {
				c.Functions = append(c.Functions, Function{Name: f.Name, Unit: f.Unit, Decorators: f.Decorators})
			}
			for _, e := range append(append([]string{}, uc.Effects...), uc.Guard...) {
				if e = strings.TrimSpace(e); e != "" {
					effects[e] = true
				}
			}
			return nil
		}
		// No unit contract here: the minimal reader's answer stands.
		for _, m := range effectAtoms.FindAllStringSubmatch(body, -1) {
			for atom := range strings.SplitSeq(m[1], ",") {
				if atom = strings.TrimSpace(atom); atom != "" {
					effects[atom] = true
				}
			}
		}
		c.Functions = append(c.Functions, contractedFunctions(rel, body)...)
		return nil
	})
	if err != nil {
		return Contract{}, fmt.Errorf("verify: read contract: %w", err)
	}
	for e := range effects {
		c.Effects = append(c.Effects, e)
	}
	sort.Strings(c.Effects)
	sort.Slice(c.Functions, func(i, j int) bool {
		if c.Functions[i].Unit != c.Functions[j].Unit {
			return c.Functions[i].Unit < c.Functions[j].Unit
		}
		return c.Functions[i].Name < c.Functions[j].Name
	})
	sort.Slice(c.Checks, func(i, j int) bool { return c.Checks[i].Name < c.Checks[j].Name })
	return c, nil
}
