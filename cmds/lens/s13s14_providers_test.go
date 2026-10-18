package lenscmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentAndASTProvidersThroughLens(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"data.json": `{"items":[{"name":"first"}]}`,
		"data.yaml": "items:\n  - name: first\n",
		"sample.go": "package sample\nfunc Example() {}\n",
		"notes.md": "# Plan\n## Next step\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ locator, leaf string }{
		{"json:data.json", "name"},
		{"yaml:data.yaml", "name"},
		{"ast:sample.go", "Example"},
		{"md:notes.md", "next-step"},
	} {
		out, errb, code := runTool(t, root, "", "-t", "paths", tc.locator)
		if code != 0 || errb != "" {
			t.Fatalf("%s: code=%d stderr=%q", tc.locator, code, errb)
		}
		if !strings.Contains(out, tc.leaf) {
			t.Fatalf("%s paths omit %q: %s", tc.locator, tc.leaf, out)
		}
	}
}

func TestSystemProvidersRegistered(t *testing.T) {
	for _, scheme := range []string{"proc", "net", "pdf"} {
		if providers[scheme] == nil {
			t.Errorf("%s provider is not registered", scheme)
		}
	}
}
