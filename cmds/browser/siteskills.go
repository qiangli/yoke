package browsercmd

// Site skills (Sprint 512): page-handling knowledge lives in ordinary dhnt
// skills whose `metadata.browser-match` names the URLs they handle, not in
// the extension. `browser skills` lists the skills matching the driven tab
// (or --url); `extract` adds a one-line hint when one matches. Running and
// healing a skill is `bashy skill run [--adapt]` — nothing here executes one.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/browser/wire"
	"github.com/qiangli/yoke/pkg/skills"
)

// SkillOptions returns the catalog options the host binary uses for
// `bashy skill`, so the browser sees the same rings (embedded, shared,
// repository, host overlay). The consumer sets it at init; it is called
// only when a lookup runs, so it costs nothing at startup. nil means the
// default catalog.
var SkillOptions func() []skills.Option

// siteSkillCatalog lists the catalog; a variable so tests supply rows.
var siteSkillCatalog = func() ([]skills.Listing, error) {
	var opts []skills.Option
	if SkillOptions != nil {
		opts = SkillOptions()
	}
	cat, ps := skills.NewCatalog(opts...)
	return cat.List(ps)
}

func init() {
	subcommands["skills"] = subcommand{
		Name: "skills", Args: "",
		One:  "list site skills whose metadata.browser-match covers the driven tab (or --url)",
		Mode: "solo, probe, live",
		Out:  "text `NAME  DESCRIPTION` per skill; --json emits {url, skills:[{name,description,match}]}",
		Body: []string{
			"A site skill is an ordinary dhnt skill with metadata.browser-match:",
			"comma-separated URL globs where * matches any run of characters.",
			"Read it with `bashy skill show NAME`, run it with `bashy skill run NAME`;",
			"a failing step is repaired by `bashy skill run --adapt NAME`.",
		},
	}
}

type siteSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Match       string `json:"match"`
}

// globMatch reports whether url matches one browser-match glob. `*` spans
// any characters, including `/`; everything else is literal.
func globMatch(glob, url string) bool {
	glob = strings.TrimSpace(glob)
	if glob == "" {
		return false
	}
	parts := strings.Split(glob, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re, err := regexp.Compile("^" + strings.Join(parts, ".*") + "$")
	return err == nil && re.MatchString(url)
}

// matchSiteSkills returns the skills whose browser-match covers url,
// sorted by name.
func matchSiteSkills(rows []skills.Listing, url string) []siteSkill {
	var out []siteSkill
	for _, r := range rows {
		for _, g := range strings.Split(r.Meta["browser-match"], ",") {
			if globMatch(g, url) {
				out = append(out, siteSkill{Name: r.Name, Description: r.Description, Match: strings.TrimSpace(g)})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func lookupSiteSkills(url string) ([]siteSkill, error) {
	rows, err := siteSkillCatalog()
	if err != nil {
		return nil, err
	}
	return matchSiteSkills(rows, url), nil
}

// withSiteSkillHint appends one hint to an extract result when a site
// skill covers the page. A catalog error never fails the extract.
func withSiteSkillHint(a wire.Action, res *wire.Result) *wire.Result {
	if res == nil || a.Type != wire.ActionExtract || res.URL == "" {
		return res
	}
	found, err := lookupSiteSkills(res.URL)
	if err != nil || len(found) == 0 {
		return res
	}
	names := make([]string, len(found))
	for i, s := range found {
		names[i] = s.Name
	}
	res.Hints = append(res.Hints, fmt.Sprintf(
		"site skill(s) for this page: %s — bashy skill show %s", strings.Join(names, ", "), names[0]))
	return res
}

// browserSkills implements `browser skills`. Without --url it reads the
// driven tab's URL through the selected backend.
func browserSkills(rc *tool.RunContext, ctx context.Context, url string, open func() (urlReader, error), asJSON bool) int {
	if url == "" {
		c, err := open()
		if err != nil {
			fmt.Fprintf(rc.Err, "browser skills: %v (pass --url to look up without a browser)\n", err)
			return 1
		}
		defer c.Close()
		res, err := c.Execute(ctx, wire.Action{Type: wire.ActionExtract, Limit: 1})
		if err != nil {
			fmt.Fprintf(rc.Err, "browser skills: %v (pass --url to look up without a browser)\n", err)
			return 1
		}
		url = res.URL
	}
	if url == "" {
		fmt.Fprintln(rc.Err, "browser skills: the driven tab reported no URL; pass --url")
		return 1
	}
	found, err := lookupSiteSkills(url)
	if err != nil {
		fmt.Fprintf(rc.Err, "browser skills: %v\n", err)
		return 1
	}
	if asJSON {
		if found == nil {
			found = []siteSkill{}
		}
		b, _ := json.Marshal(struct {
			URL    string      `json:"url"`
			Skills []siteSkill `json:"skills"`
		}{url, found})
		fmt.Fprintln(rc.Out, string(b))
		return 0
	}
	if len(found) == 0 {
		fmt.Fprintf(rc.Err, "browser skills: no site skill matches %s\n", url)
		return 0
	}
	for _, s := range found {
		fmt.Fprintf(rc.Out, "%s\t%s\n", s.Name, s.Description)
	}
	return 0
}

// urlReader is the slice of a browser client `browser skills` needs.
type urlReader interface {
	Execute(ctx context.Context, a wire.Action) (*wire.Result, error)
	Close() error
}
