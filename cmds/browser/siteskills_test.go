package browsercmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/browser/wire"
	"github.com/qiangli/yoke/pkg/skills"
)

func row(name, match string) skills.Listing {
	return skills.Listing{Skill: skills.Skill{Name: name, Description: name + " desc", Meta: map[string]string{"browser-match": match}}}
}

func withRows(t *testing.T, rows ...skills.Listing) {
	t.Helper()
	old := siteSkillCatalog
	siteSkillCatalog = func() ([]skills.Listing, error) { return rows, nil }
	t.Cleanup(func() { siteSkillCatalog = old })
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, url string
		want      bool
	}{
		{"https://*.typeform.com/*", "https://acme.typeform.com/to/abc?x=1", true},
		{"https://*.typeform.com/*", "https://typeform.com.evil.io/x", false},
		{"https://portal.azure.com/*", "https://portal.azure.com/#home", true},
		{"https://portal.azure.com/*", "http://portal.azure.com/", false},
		{"", "https://x/", false},
		{"https://a.b/c", "https://a.b/c", true},
		{"https://a.b/c", "https://a.b/cd", false},
	}
	for _, c := range cases {
		if got := globMatch(c.glob, c.url); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.glob, c.url, got, c.want)
		}
	}
}

func TestMatchSiteSkills(t *testing.T) {
	rows := []skills.Listing{
		row("typeform-fill", "https://*.typeform.com/*"),
		row("aws-startups", "https://aws.amazon.com/startups/*, https://*.awsapps.com/*"),
		row("plain", ""),
	}
	got := matchSiteSkills(rows, "https://x.awsapps.com/start")
	if len(got) != 1 || got[0].Name != "aws-startups" || got[0].Match != "https://*.awsapps.com/*" {
		t.Fatalf("got %+v", got)
	}
	if got := matchSiteSkills(rows, "https://example.com/"); len(got) != 0 {
		t.Fatalf("unrelated URL matched %+v", got)
	}
}

func TestExtractHintNamesMatchingSkill(t *testing.T) {
	withRows(t, row("typeform-fill", "https://*.typeform.com/*"))
	res := withSiteSkillHint(wire.Action{Type: wire.ActionExtract}, &wire.Result{URL: "https://a.typeform.com/to/x"})
	if len(res.Hints) != 1 || !strings.Contains(res.Hints[0], "typeform-fill") {
		t.Fatalf("hints = %q", res.Hints)
	}
	res = withSiteSkillHint(wire.Action{Type: wire.ActionClick}, &wire.Result{URL: "https://a.typeform.com/to/x"})
	if len(res.Hints) != 0 {
		t.Fatalf("non-extract action got a hint: %q", res.Hints)
	}
}

func TestBrowserSkillsByURLJSON(t *testing.T) {
	withRows(t, row("typeform-fill", "https://*.typeform.com/*"))
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Out: &out, Err: &errb}
	noBrowser := func() (urlReader, error) { t.Fatal("--url must not open a browser"); return nil, nil }
	if code := browserSkills(rc, context.Background(), "https://a.typeform.com/to/x", noBrowser, true); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var env struct {
		URL    string      `json:"url"`
		Skills []siteSkill `json:"skills"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Skills) != 1 || env.Skills[0].Name != "typeform-fill" {
		t.Fatalf("env = %+v", env)
	}
}

type fakeTab struct{ url string }

func (f fakeTab) Execute(context.Context, wire.Action) (*wire.Result, error) {
	return &wire.Result{Success: true, URL: f.url}, nil
}
func (fakeTab) Close() error { return nil }

func TestBrowserSkillsReadsDrivenTab(t *testing.T) {
	withRows(t, row("typeform-fill", "https://*.typeform.com/*"))
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Out: &out, Err: &errb}
	open := func() (urlReader, error) { return fakeTab{"https://a.typeform.com/to/x"}, nil }
	if code := browserSkills(rc, context.Background(), "", open, false); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "typeform-fill\t") {
		t.Fatalf("out = %q", out.String())
	}
}
