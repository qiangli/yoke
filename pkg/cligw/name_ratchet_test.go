package cligw

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNoToolNameBranches(t *testing.T) {
	re := regexp.MustCompile(`tool\.Name\s*==|case\s+"(?:claude|codex|agy|muse)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if match := re.Find(body); match != nil {
			t.Errorf("%s retains tool-name branch %q", file, match)
		}
	}
}
