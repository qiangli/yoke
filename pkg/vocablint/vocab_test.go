package vocablint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Blocking vocabulary lint for yoke's cobra surface, checked from source so
// every command is covered without constructing it.

var flagCall = regexp.MustCompile(`^(Bool|String|Int|Int64|Float64|Duration|StringSlice|StringArray)(Var)?P?$`)

var badSubverbs = map[string]string{
	"ls": "list", "create": "add", "get": "show", "remove": "rm",
	"delete": "rm", "del": "rm", "new": "add", "update": "set",
}

var badFlags = map[string]string{
	"dryrun": "dry-run", "dry_run": "dry-run", "noinput": "no-input",
}

var subverbDirs = []string{"pkg/broker/", "pkg/feed/", "pkg/secrets/"}

func lintFile(fset *token.FileSet, path string, src any) []string {
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil
	}
	scoped := false
	for _, d := range subverbDirs {
		if strings.Contains(filepath.ToSlash(path), d) {
			scoped = true
		}
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.KeyValueExpr:
			id, ok := x.Key.(*ast.Ident)
			if !ok || id.Name != "Use" || !scoped {
				return true
			}
			if s, ok := strLit(x.Value); ok {
				verb := strings.Fields(s + " ")[0]
				if want, bad := badSubverbs[verb]; bad {
					out = append(out, fset.Position(x.Pos()).String()+": subverb "+verb+" must be "+want)
				}
			}
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok || !flagCall.MatchString(sel.Sel.Name) {
				return true
			}
			base := strings.TrimSuffix(strings.TrimSuffix(sel.Sel.Name, "P"), "Var")
			for _, a := range x.Args {
				name, ok := strLit(a)
				if !ok {
					continue
				}
				at := fset.Position(x.Pos()).String()
				if want, bad := badFlags[name]; bad {
					out = append(out, at+": --"+name+" must be --"+want)
				}
				if (name == "force" || name == "yes" || name == "quiet") && base != "Bool" {
					out = append(out, at+": --"+name+" must be boolean (idle windows are --idle)")
				}
				break
			}
		}
		return true
	})
	return out
}

func TestVocabularyLintBlocking(t *testing.T) {
	fset := token.NewFileSet()
	skip := map[string]bool{"external": true, ".git": true, "testdata": true}
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			for _, f := range lintFile(fset, path, nil) {
				t.Error(f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVocabularyLintFlagsOffVocabulary(t *testing.T) {
	src := `package x
func f() {
	c := &cobra.Command{Use: "ls"}
	f.BoolVarP(&a, "dryrun", "n", false, "")
	f.DurationVar(&q, "quiet", 0, "")
	f.StringVar(&y, "yes", "", "")
}`
	got := lintFile(token.NewFileSet(), "pkg/feed/x.go", src)
	if len(got) != 4 {
		t.Fatalf("want 4 findings, got %d: %v", len(got), got)
	}
	ok := `package x
func f() {
	c := &cobra.Command{Use: "list"}
	f.BoolVar(&a, "dry-run", false, "")
	f.DurationVar(&q, "idle", 0, "")
	f.BoolVar(&y, "yes", false, "")
}`
	if got := lintFile(token.NewFileSet(), "pkg/feed/x.go", ok); len(got) != 0 {
		t.Fatalf("clean source flagged: %v", got)
	}
}

func strLit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}
