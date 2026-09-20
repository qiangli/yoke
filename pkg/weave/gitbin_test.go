package weave

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// No bashy code in this package may exec a bare "git": every site goes
// through gitBin (gitscm.Path), the git bashy provisions — MinGit on Windows,
// where nothing puts a git.exe on PATH. Found live on the first Windows run
// of Sprint 217 ("repo has no origin remote" from a host where `bashy git`
// worked fine).
func TestNoBareGitExecInWeave(t *testing.T) {
	bare := regexp.MustCompile(`exec\.Command(Context)?\((\w+, )?"git"`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if loc := bare.FindIndex(b); loc != nil {
			line := 1 + strings.Count(string(b[:loc[0]]), "\n")
			t.Errorf("%s:%d execs a bare \"git\"; use gitBin()", e.Name(), line)
		}
	}
}
