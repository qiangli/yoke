package skills

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseFrontmatterType(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantType string
		wantWarn bool
	}{
		{
			name:     "top-level only",
			doc:      "---\nname: build\ndescription: Build tasks.\ntype: dag\n---\nbody\n",
			wantType: "dag",
		},
		{
			name:     "metadata only",
			doc:      "---\nname: form-pilot\ndescription: Shapeware form pilot.\nmetadata:\n  type: shapeware\n---\nbody\n",
			wantType: "shapeware",
		},
		{
			name:     "both equal",
			doc:      "---\nname: form-pilot\ndescription: Shapeware form pilot.\ntype: shapeware\nmetadata:\n  type: shapeware\n---\nbody\n",
			wantType: "shapeware",
		},
		{
			name:     "both differ",
			doc:      "---\nname: form-pilot\ndescription: Shapeware form pilot.\ntype: dag\nmetadata:\n  type: shapeware\n---\nbody\n",
			wantType: "shapeware",
			wantWarn: true,
		},
		{
			name:     "neither",
			doc:      "---\nname: plain\ndescription: Plain skill.\n---\nbody\n",
			wantType: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sk, err := ParseFrontmatter([]byte(tc.doc))
			if err != nil {
				t.Fatalf("ParseFrontmatter: %v", err)
			}
			if sk.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", sk.Type, tc.wantType)
			}
			if tc.wantWarn {
				if sk.TypeWarning == "" {
					t.Errorf("TypeWarning = %q, want non-empty warning", sk.TypeWarning)
				}
				if !strings.Contains(sk.TypeWarning, "shapeware") || !strings.Contains(sk.TypeWarning, "dag") {
					t.Errorf("TypeWarning = %q, want both conflicting types mentioned", sk.TypeWarning)
				}
			} else if sk.TypeWarning != "" {
				t.Errorf("TypeWarning = %q, want empty", sk.TypeWarning)
			}
		})
	}
}

func TestCatalogListSurfacesTypeWarning(t *testing.T) {
	embedded := fstest.MapFS{
		"conflict/SKILL.md":  {Data: []byte("---\nname: conflict\ndescription: conflicting type keys\ntype: dag\nmetadata:\n  type: shapeware\n---\nbody\n")},
		"clean/SKILL.md":     {Data: []byte("---\nname: clean\ndescription: matching type keys\ntype: shapeware\nmetadata:\n  type: shapeware\n---\nbody\n")},
		"both-warn/SKILL.md": {Data: []byte("---\nname: both-warn\ndescription: conflict and bad requires\ntype: dag\nmetadata:\n  type: shapeware\n  requires: \"os=\"\n---\nbody\n")},
	}
	cat := &Catalog{Sources: []Source{EmbedSource(embedded, RingEmbedded)}}
	ps := testProbes(t, map[string]string{"os": "linux", "arch": "arm64"}, nil)
	rows, err := cat.List(ps)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Listing{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	if r := byName["conflict"]; r.Type != "shapeware" || !r.Verdict.Applicable || r.Warning == "" {
		t.Errorf("conflict: type=%q applicable=%v warning=%q", r.Type, r.Verdict.Applicable, r.Warning)
	}
	if r := byName["clean"]; r.Type != "shapeware" || !r.Verdict.Applicable || r.Warning != "" {
		t.Errorf("clean: type=%q applicable=%v warning=%q", r.Type, r.Verdict.Applicable, r.Warning)
	}
	if r := byName["both-warn"]; r.Type != "shapeware" || !strings.Contains(r.Warning, "requires unparsable") || !strings.Contains(r.Warning, "metadata.type") {
		t.Errorf("both-warn: type=%q warning=%q", r.Type, r.Warning)
	}
}
