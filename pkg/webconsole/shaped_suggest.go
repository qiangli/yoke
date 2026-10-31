// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"net/http"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/qiangli/yoke/pkg/shape/appserver"
)

// Example wishes for the wish form (Sprint 526.B1g, operator 2026-10-31,
// docs/sprint-526/genie-prompt-and-suggestions-2026-10-31.md, revision of
// the same day). The form's prompt is one fixed, generic line for every
// app and page — "What would you like to change, add or make?" — and what
// fits THIS app is shown as the textarea's placeholder: example wishes the
// component fetches from GET api/shape/<app>/suggest?page=<page> when the
// panel opens and rotates through the placeholder. Nothing is prefilled,
// nothing is submitted until the person types.
//
// Suggestions must be quick and must not read the whole app, so opening
// the panel makes NO model call. The answer comes from the cheapest source
// that has one, in this order:
//
//  1. The follow-up wishes Genie listed when it finished the last wish.
//     The shaping brief asks for NEXT: lines beside COVERED / NOT COVERED
//     (shapingTaskFor); parseNextSteps reads them like the coverage list;
//     they are stored with that version as Shape-Next trailers of its
//     commit (commitCandidate) and on the candidate / draft-step record,
//     and the next panel open shows them.
//  2. Follow-ups derived from the version's own wish (its Shape-Request
//     trailer) when it has no saved next steps — older versions, an agent
//     that committed itself: "Change <the thing just added>" and "Undo
//     <the last wish>". No model call.
//  3. The cached copy of what this app and page last showed this
//     principal (in memory, per process) — a restore to the base, say,
//     keeps the last suggestions rather than none. Per principal: a
//     derived wish is the principal's own text.
//  4. Nothing: the component keeps its neutral "Describe what you'd like".
//
// An app may declare its own fixed examples in shaped.json (genie.examples)
// — used as they are, before every other source. Protected pages get the
// same suggestions (a wish from there is filed, not built).

const (
	// nextHeading is the heading of Genie's follow-up list in its answer.
	nextHeading = "NEXT:"
	// nextTrailer is the commit trailer one follow-up wish is stored under.
	nextTrailer = "Shape-Next:"
	// requestTrailer is the commit trailer carrying the wish that made the
	// version (commitCandidate).
	requestTrailer = "Shape-Request:"
	// suggestPageLimit bounds the page parameter.
	suggestPageLimit = geniePageLimit
	// suggestCacheMax bounds the cache; past it the whole map is dropped
	// (entries are cheap to rebuild).
	suggestCacheMax = 512
)

// suggestKey names one cached answer.
type suggestKey struct {
	app, principal, page string
}

// handleShapeSuggest is GET /api/shape/{app}/suggest?page=<page>[&draft=1]
// → {"examples": [...]}. Same admission as every Shape route (shapeCaller).
// With draft=1 the suggestions follow the open draft's last applied step
// (what the person sees in the edit session) instead of the active version.
func (s *server) handleShapeSuggest(w http.ResponseWriter, r *http.Request) {
	a, principal, ok := s.shapeCaller(w, r)
	if !ok {
		return
	}
	page := suggestPage(r.URL.Query().Get("page"))
	w.Header().Set("Cache-Control", "no-store")
	var examples []string
	if repo, err := a.open(); err == nil {
		if c, err := a.active(repo, principal); err == nil {
			tree, _ := c.Tree()
			if cfg, _ := genieConfigOf(tree); len(cfg.Examples) > 0 {
				writeJSON(w, http.StatusOK, map[string]any{"examples": cfg.Examples})
				return
			}
			examples = versionSuggestions(c)
		}
	}
	if r.URL.Query().Get("draft") == "1" {
		if d, _, err := a.loadDraft(principal); err == nil && d.Cursor > 0 {
			examples = stepSuggestions(d.Steps[d.Cursor-1])
		}
	}
	examples = s.rememberSuggestions(suggestKey{app: a.name, principal: principal, page: page}, examples)
	writeJSON(w, http.StatusOK, map[string]any{"examples": examples})
}

// versionSuggestions is sources 1 and 2 for a version: its saved
// follow-ups, else follow-ups derived from its wish. A version with
// neither (the base, a release) answers nil.
func versionSuggestions(c *object.Commit) []string {
	if c == nil {
		return nil
	}
	if next := trailerValues(c.Message, nextTrailer); len(next) > 0 {
		return cleanWishes(next)
	}
	if req := trailerValues(c.Message, requestTrailer); len(req) > 0 {
		return deriveFollowUps(req[len(req)-1])
	}
	return nil
}

// stepSuggestions is the same for a draft step, from its record.
func stepSuggestions(st draftStep) []string {
	if len(st.Next) > 0 {
		return cleanWishes(st.Next)
	}
	return deriveFollowUps(st.Request)
}

// rememberSuggestions is source 3: a non-empty answer replaces the cached
// copy for the key; an empty one is answered from it. Never nil in the
// response (the component wants an array).
func (s *server) rememberSuggestions(key suggestKey, examples []string) []string {
	hub := &s.shapes
	hub.suggestMu.Lock()
	defer hub.suggestMu.Unlock()
	if hub.suggestions == nil || len(hub.suggestions) > suggestCacheMax {
		hub.suggestions = map[suggestKey][]string{}
	}
	if len(examples) > 0 {
		hub.suggestions[key] = append([]string(nil), examples...)
	} else {
		examples = hub.suggestions[key]
	}
	out := make([]string, len(examples))
	copy(out, examples)
	return out
}

// suggestPage normalises the page parameter: the path alone, cleaned,
// rooted and bounded.
func suggestPage(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	dir := strings.HasSuffix(p, "/")
	p = path.Clean("/" + p)
	if dir && p != "/" {
		p += "/"
	}
	if len(p) > suggestPageLimit {
		p = p[:suggestPageLimit]
	}
	return p
}

// trailerValues returns every value of key in the trailer block of a
// commit message (the last paragraph), in order.
func trailerValues(msg, key string) []string {
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	start := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			break
		}
		start = i
	}
	var out []string
	for _, ln := range lines[start:] {
		if strings.HasPrefix(ln, key) {
			if v := strings.TrimSpace(strings.TrimPrefix(ln, key)); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// parseNextSteps reads Genie's follow-up list: the bullets under the last
// NEXT: heading, up to the next heading or blank line, cleaned like a
// declared example list. "- (none)" and prose are dropped.
func parseNextSteps(output string) []string {
	var items []string
	in := false
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		heading := strings.TrimSpace(strings.Trim(line, "#* "))
		if heading == nextHeading {
			items, in = nil, true
			continue
		}
		if !in {
			continue
		}
		if line == "" || strings.HasSuffix(heading, ":") {
			in = false
			continue
		}
		if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") {
			continue
		}
		items = append(items, line[2:])
	}
	return cleanWishes(items)
}

var (
	reSpace = regexp.MustCompile(`\s+`)
	// reBullet is a list marker at the start of a line.
	reBullet = regexp.MustCompile(`^(?:[-*•·>#]+|\d{1,2}[.)])\s*`)
	// reLeadVerb is the imperative a wish commonly opens with; what follows
	// is the thing it is about.
	reLeadVerb = regexp.MustCompile(`(?i)^(?:please\s+)?(?:add|make|show|put|create|build|give|display|include|insert|use|let|have|turn)\s+(?:me\s+)?`)
)

// cleanWishes tidies a list of wishes the way a declared list is bounded:
// markers and quotes removed, whitespace collapsed, markup and overlong or
// empty lines dropped, duplicates folded, at most GenieExamplesMax.
func cleanWishes(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		v = reBullet.ReplaceAllString(strings.TrimSpace(v), "")
		v = strings.Trim(strings.TrimSpace(v), "\"'`“”‘’")
		v = strings.TrimSpace(reSpace.ReplaceAllString(v, " "))
		if v == "" || strings.HasSuffix(v, ":") || len(v) > appserver.GenieExampleLen || strings.ContainsAny(v, "<>") {
			continue
		}
		if r := []rune(v); len(r) < 3 || !(unicode.IsLetter(r[0]) || unicode.IsDigit(r[0])) {
			continue
		}
		if strings.EqualFold(v, "(none)") || strings.EqualFold(v, "none") {
			continue
		}
		k := strings.ToLower(v)
		if seen[k] || len(out) >= appserver.GenieExamplesMax {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}

// deriveFollowUps is source 2: simple follow-ups from the wish that made
// the version — "Change <the thing>" (the wish without its leading verb,
// "a"/"an" read as "the") and "Undo <the wish>". Empty for an empty wish.
func deriveFollowUps(request string) []string {
	w := strings.TrimSpace(reSpace.ReplaceAllString(request, " "))
	w = strings.TrimRight(w, ".!")
	if w == "" {
		return nil
	}
	thing := reLeadVerb.ReplaceAllString(w, "")
	if thing == "" {
		thing = w
	}
	if f := strings.Fields(thing); len(f) > 1 && (strings.EqualFold(f[0], "a") || strings.EqualFold(f[0], "an")) {
		thing = "the " + strings.Join(f[1:], " ")
	}
	return cleanWishes([]string{"Change " + lowerFirst(thing), "Undo " + lowerFirst(w)})
}

// lowerFirst lowers the first rune unless the word is all capitals (an
// acronym, a name in caps).
func lowerFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	first := strings.FieldsFunc(s, unicode.IsSpace)
	if len(first) > 0 && len([]rune(first[0])) > 1 && strings.ToUpper(first[0]) == first[0] {
		return s
	}
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
