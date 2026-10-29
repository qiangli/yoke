package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ContractReader reads the shaping contract out of one checked-out tree.
// It is an interface so tests can stand in a fixed contract without
// touching the verifier: the verifier only ever needs the contracted
// functions, the required checks and the effect cap. Production uses
// UnitReader, the pkg/unit-backed reader.
type ContractReader interface {
	// ReadContract reads the contract from the tree rooted at dir.
	ReadContract(dir string) (Contract, error)
}

// Function is one contracted function: a function definition carrying at
// least one decorator. Every contracted function in the base contract must
// be invoked by the check set, or the verdict fails.
type Function struct {
	// Name is the shell function name.
	Name string `json:"name"`
	// Unit is the slash-separated path of the file that declares it.
	Unit string `json:"unit"`
	// Decorators are the contract lines above the definition, in order and
	// trimmed. They are part of the contract digest: editing a
	// precondition or postcondition changes what was verified.
	Decorators []string `json:"decorators"`
}

// Check is one required check: a unit under checks/ whose body the verifier
// freezes and runs. Body is the bytes as read from the tree it came from.
type Check struct {
	Name string `json:"name"`
	Body string `json:"-"`
	// BodyDigest stands in for Body in the contract digest.
	BodyDigest string `json:"body"`
}

// Contract is one tree's shaping contract.
type Contract struct {
	// Functions are the contracted functions, sorted by unit then name.
	Functions []Function `json:"functions"`
	// Checks are the required checks, sorted by name.
	Checks []Check `json:"checks"`
	// Effects is the union of the effect atoms the tree declares, sorted.
	// Read from the base it is the cap; read from a candidate it is the
	// claim measured against that cap.
	Effects []string `json:"effects"`
}

// CheckDir is the tree-relative directory the minimal reader takes required
// checks from.
const CheckDir = "checks"

// checksRoot is CheckDir with a trailing slash, for prefix tests.
const checksRoot = CheckDir + "/"

// Digest is the contract's stable fingerprint: the same contract always
// digests the same way, and any change to a decorator line, a check body,
// a contracted function or the declared effects changes it. It is the
// contract half of an evidence binding.
func (c Contract) Digest() string {
	var b strings.Builder
	b.WriteString("shape-contract-v1\n")
	b.WriteString("effects\t" + strings.Join(c.Effects, ",") + "\n")
	for _, f := range c.Functions {
		b.WriteString("fn\t" + f.Unit + "\t" + f.Name + "\n")
		for _, d := range f.Decorators {
			b.WriteString("\t" + d + "\n")
		}
	}
	for _, ch := range c.Checks {
		b.WriteString("check\t" + ch.Name + "\t" + ch.BodyDigest + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FunctionNames returns the contracted function names, sorted and
// deduplicated — the accounting set.
func (c Contract) FunctionNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range c.Functions {
		if !seen[f.Name] {
			seen[f.Name] = true
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Cap renders the declared effects as a contain effect-cap spec.
func (c Contract) Cap() string { return strings.Join(c.Effects, ",") }

// DecoratorReader is the minimal contract reader: it reads @require,
// @ensure, @guard and @effects decorators out of the tree's Bash# units.
// A unit is a .bsh/.bash file or a Markdown document with bsh fences.
//
// It is deliberately small, and it stays as the fallback inside UnitReader
// for files where pkg/unit finds no contract.
type DecoratorReader struct {
	// Exts are the file extensions read as raw Bash# bodies.
	// Empty means .bsh and .bash.
	Exts []string
	// FenceExts are the extensions read as fenced documents. Empty means
	// .md.
	FenceExts []string
}

// decoratorLine matches a contract decorator at the start of a line. The
// four names are the Bash# contract vocabulary.
var decoratorLine = regexp.MustCompile(`^\s*@(require|ensure|guard|effects)\b`)

// funcLine matches a POSIX shell function definition header.
var funcLine = regexp.MustCompile(`^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_:.-]*)\s*\(\s*\)\s*\{?`)

// effectAtoms pulls the atoms out of @guard(effects: "a,b") and
// @effects("a,b") in both the keyword and the positional spelling.
var effectAtoms = regexp.MustCompile(`@(?:guard\s*\(\s*effects\s*:|effects\s*\()\s*["']([^"']*)["']`)

// ReadContract walks dir and returns its contract.
func (r DecoratorReader) ReadContract(dir string) (Contract, error) {
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
			body = bshFences(body)
			if strings.TrimSpace(body) == "" {
				return nil
			}
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		for _, m := range effectAtoms.FindAllStringSubmatch(body, -1) {
			for atom := range strings.SplitSeq(m[1], ",") {
				if atom = strings.TrimSpace(atom); atom != "" {
					effects[atom] = true
				}
			}
		}
		if strings.HasPrefix(rel, checksRoot) {
			sum := sha256.Sum256([]byte(body))
			c.Checks = append(c.Checks, Check{
				Name: rel, Body: body, BodyDigest: "sha256:" + hex.EncodeToString(sum[:]),
			})
			return nil
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

// contractedFunctions pairs each run of decorator lines with the function
// definition that follows it. A decorator run with no definition after it
// contracts nothing and is ignored: a contract without a subject is not a
// contract.
func contractedFunctions(unit, body string) []Function {
	var out []Function
	var pending []string
	for line := range strings.SplitSeq(body, "\n") {
		if decoratorLine.MatchString(line) {
			pending = append(pending, strings.TrimSpace(line))
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(pending) > 0 {
			if m := funcLine.FindStringSubmatch(line); m != nil {
				out = append(out, Function{Name: m[1], Unit: unit, Decorators: pending})
			}
		}
		pending = nil
	}
	return out
}

// bshFences returns the concatenated bsh/bashsharp fenced blocks of a
// document, preserving bytes. An unterminated fence yields nothing: a
// half-written document is not an executable unit.
func bshFences(document string) string {
	var body strings.Builder
	var fence string
	var selected bool
	for _, line := range strings.SplitAfter(document, "\n") {
		trim := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trim, fence) && strings.Trim(trim, string(fence[0])) == "" {
				fence, selected = "", false
			} else if selected {
				body.WriteString(line)
			}
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			n := 0
			for n < len(trim) && trim[n] == trim[0] {
				n++
			}
			fence = trim[:n]
			tag := strings.TrimSpace(trim[n:])
			selected = tag == "bsh" || tag == "bashsharp"
		}
	}
	if fence != "" && selected {
		return ""
	}
	return body.String()
}
