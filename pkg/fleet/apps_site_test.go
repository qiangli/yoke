package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSiteAppValidation(t *testing.T) {
	source := t.TempDir()
	valid := App{Name: "docs", Type: AppTypeSite, Source: source}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid site: %v", err)
	}

	cases := []struct {
		name string
		app  App
		want string
	}{
		{"missing source", App{Name: "docs", Type: AppTypeSite}, "absolute --source directory"},
		{"relative source", App{Name: "docs", Type: AppTypeSite, Source: "site"}, "absolute --source directory"},
		{"missing directory", App{Name: "docs", Type: AppTypeSite, Source: filepath.Join(source, "missing")}, "site source"},
		{"port", App{Name: "docs", Type: AppTypeSite, Source: source, Port: 8080}, "has no port"},
		{"start", App{Name: "docs", Type: AppTypeSite, Source: source, Start: []string{"serve"}}, "has no start argv"},
		{"shaping", App{Name: "docs", Type: AppTypeSite, Source: source, Shaping: &AppShaping{}}, "requires type shaped"},
		{"unknown type", App{Name: "docs", Type: "mystery"}, "want proxy, shaped or site"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.app.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want containing %q", err, tc.want)
			}
		})
	}

	file := filepath.Join(source, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (App{Name: "docs", Type: AppTypeSite, Source: file}).Validate(); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file source = %v", err)
	}
}

func TestSiteAppCLIAddShowSchemaAndSetValidation(t *testing.T) {
	isolatedFleetRoot(t)
	root, source := t.TempDir(), t.TempDir()
	opts := []Option{WithRoot(root)}

	out, err := runCmd(t, appRoot(opts), "add", "docs", "--type", "site", "--source", source)
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(site, "+source+")") {
		t.Fatalf("add output = %q", out)
	}
	a, ok := New(opts...).App("docs")
	if !ok || a.Type != AppTypeSite || a.Source != source {
		t.Fatalf("record = %+v, %v", a, ok)
	}
	show, err := runCmd(t, appRoot(opts), "show", "docs")
	if err != nil || !strings.Contains(show, "type: site") || !strings.Contains(show, "source: "+source) {
		t.Fatalf("show: %v\n%s", err, show)
	}
	schema, err := runCmd(t, appRoot(opts), "schema", "--json")
	if err != nil || !strings.Contains(schema, "live static directory") {
		t.Fatalf("schema: %v\n%s", err, schema)
	}
	if _, err := runCmd(t, appRoot(opts), "set", "docs", "--source", "relative"); err == nil || !strings.Contains(err.Error(), "absolute --source directory") {
		t.Fatalf("set accepted a relative source: %v", err)
	}
	if _, err := runCmd(t, appRoot(opts), "set", "docs", "--port", "8080"); err == nil || !strings.Contains(err.Error(), "has no port") {
		t.Fatalf("set accepted a site port: %v", err)
	}
}
