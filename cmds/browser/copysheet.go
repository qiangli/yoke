package browsercmd

// `browser copysheet` — a prepared copy/paste page for a form bashy can read
// but not fill (Sprint 529, story S529.6). Some pages refuse every extension
// (the Chrome Web Store, many banks), so the agent prepares each field's
// value and the human pastes it: the sheet opens beside the live page, one
// Copy button per field, and a copied field greys out to show progress.
//
// Guard parity: a value the sensitive-input guard would refuse to type is
// never written to the sheet. Its field says "enter this yourself" and is
// reported as withheld, so an agent cannot stage a secret for pasting.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/browser/live"
	"github.com/qiangli/yoke/pkg/browser/wire"
	"github.com/qiangli/yoke/pkg/sensitive"
)

type copyField struct {
	Section string `json:"section,omitempty"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Note    string `json:"note,omitempty"`
}

type copySection struct {
	Name   string      `json:"name"`
	Fields []copyField `json:"fields"`
}

// copySheet is the input: either sections, or a flat fields list whose
// optional `section` groups them (in order of first appearance).
type copySheet struct {
	Title    string        `json:"title"`
	Intro    string        `json:"intro"`
	Sections []copySection `json:"sections"`
	Fields   []copyField   `json:"fields"`
}

type withheldField struct {
	Section string `json:"section,omitempty"`
	Label   string `json:"label"`
	Reason  string `json:"reason"`
}

// guardedLabel matches field labels the guard treats as secrets whatever the
// value looks like (a password has no checkable shape).
var guardedLabel = regexp.MustCompile(`(?i)\b(pass(word|phrase|code)|pin|one[- ]?time (code|password)|otp|security code|cvv|cvc|ssn|social security|card number|routing number|account number|secret|api[- ]?key|access token)\b`)

func readCopySheet(rc *tool.RunContext, src string) (*copySheet, error) {
	var r io.Reader
	if src == "-" {
		r = rc.In
	} else {
		p := src
		if !filepath.IsAbs(p) && rc.Dir != "" {
			p = filepath.Join(rc.Dir, p)
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var s copySheet
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	if len(s.Fields) > 0 {
		idx := map[string]int{}
		for _, f := range s.Fields {
			i, ok := idx[f.Section]
			if !ok {
				i = len(s.Sections)
				idx[f.Section] = i
				s.Sections = append(s.Sections, copySection{Name: f.Section})
			}
			s.Sections[i].Fields = append(s.Sections[i].Fields, f)
		}
		s.Fields = nil
	}
	n := 0
	for _, sec := range s.Sections {
		n += len(sec.Fields)
	}
	if n == 0 {
		return nil, fmt.Errorf("%s has no fields", src)
	}
	if s.Title == "" {
		s.Title = "Copy sheet"
	}
	return &s, nil
}

// withhold blanks every value the guard would refuse and returns what it
// withheld (labels and reasons only, never the value).
func withhold(s *copySheet) []withheldField {
	var out []withheldField
	for i := range s.Sections {
		for j := range s.Sections[i].Fields {
			f := &s.Sections[i].Fields[j]
			reason := ""
			if k := sensitive.CheckValue(f.Value); k != "" {
				reason = "value:" + k
			} else if guardedLabel.MatchString(f.Label) {
				reason = "field:label"
			}
			if reason != "" {
				f.Value = ""
				out = append(out, withheldField{Section: s.Sections[i].Name, Label: f.Label, Reason: reason})
			}
		}
	}
	return out
}

func renderCopySheet(s *copySheet, withheld []withheldField, live bool) string {
	held := map[string]string{}
	for _, w := range withheld {
		held[w.Section+"\x00"+w.Label] = w.Reason
	}
	var b strings.Builder
	esc := html.EscapeString
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>`)
	b.WriteString(esc(s.Title))
	b.WriteString(`</title>`)
	if live {
		// A queued sheet grows while the agent works; reload to pick up new
		// fields (progress and scroll survive in browser storage).
		b.WriteString(`<meta http-equiv="refresh" content="3">`)
	}
	b.WriteString(`<style>
:root{--bg:#F6F5FA;--card:#FFFFFF;--ink:#1E1B2E;--mute:#4A4760;--acc:#6D4AFF;--ok:#2E7D32;--warn:#B3261E}
@media (prefers-color-scheme:dark){:root{--bg:#15131F;--card:#1E1B2E;--ink:#F2F0FA;--mute:#BDB8D6;--acc:#9C85FF;--ok:#7BC67E;--warn:#F2B8B5}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:860px;margin:0 auto;padding:20px 16px 48px}h1{font-size:21px;margin:0 0 4px}.intro{color:var(--mute);margin:0 0 8px;white-space:pre-wrap}
.progress{color:var(--mute);font-size:13px;margin:0 0 12px}h2{font-size:16px;margin:24px 0 6px;color:var(--acc)}
.f{background:var(--card);border-radius:10px;padding:10px 14px;margin:8px 0;box-shadow:0 1px 3px #0002;transition:opacity .2s}
.f.done{opacity:.5}.h{display:flex;justify-content:space-between;align-items:center;gap:12px}.h b{overflow-wrap:anywhere}
pre{white-space:pre-wrap;overflow-wrap:anywhere;font:14px/1.5 system-ui,sans-serif;margin:6px 0 0}.note{color:var(--mute);font-size:13px;margin-top:4px}
.held{color:var(--warn);font-size:14px;margin-top:6px}
button{flex:none;border:0;border-radius:6px;background:var(--acc);color:#fff;padding:6px 14px;font:inherit;cursor:pointer}button.ok{background:var(--ok)}
</style></head><body><main><h1>`)
	b.WriteString(esc(s.Title))
	b.WriteString(`</h1>`)
	if s.Intro != "" {
		fmt.Fprintf(&b, `<p class="intro">%s</p>`, esc(s.Intro))
	}
	b.WriteString(`<p class="progress" id="progress"></p>`)
	n := 0
	for _, sec := range s.Sections {
		if sec.Name != "" {
			fmt.Fprintf(&b, `<h2>%s</h2>`, esc(sec.Name))
		}
		for _, f := range sec.Fields {
			if reason, ok := held[sec.Name+"\x00"+f.Label]; ok {
				fmt.Fprintf(&b, `<div class="f"><div class="h"><b>%s</b></div><div class="held">Enter this yourself: bashy does not prepare guarded values (%s).</div>`, esc(f.Label), esc(reason))
			} else {
				fmt.Fprintf(&b, `<div class="f" data-copy data-k="%s"><div class="h"><b>%s</b><button type="button" data-i="%d">Copy</button></div><pre id="v%d">%s</pre>`, esc(f.Label), esc(f.Label), n, n, esc(f.Value))
				n++
			}
			if f.Note != "" {
				fmt.Fprintf(&b, `<div class="note">%s</div>`, esc(f.Note))
			}
			b.WriteString(`</div>`)
		}
	}
	b.WriteString(`</main><script>
const KEY='bashy-copysheet:'+location.pathname;
function load(){try{return JSON.parse(localStorage.getItem(KEY)||'{}')}catch(e){return {}}}
function save(d){try{localStorage.setItem(KEY,JSON.stringify(d))}catch(e){}}
const state=load();
document.querySelectorAll('[data-copy]').forEach(f=>{const v=f.querySelector('pre').textContent;if(state[f.dataset.k]===v)f.classList.add('done')});
try{const y=sessionStorage.getItem(KEY+':y');if(y)scrollTo(0,+y);addEventListener('scroll',()=>sessionStorage.setItem(KEY+':y',scrollY))}catch(e){}
const total=document.querySelectorAll('[data-copy]').length;
function progress(){const d=document.querySelectorAll('[data-copy].done').length;document.getElementById('progress').textContent=d+' of '+total+' copied';}
function fallback(t){const a=document.createElement('textarea');a.value=t;document.body.appendChild(a);a.select();document.execCommand('copy');a.remove();}
document.addEventListener('click',e=>{const btn=e.target.closest('button[data-i]');if(!btn)return;
 const t=document.getElementById('v'+btn.dataset.i).textContent;
 const done=()=>{btn.textContent='Copied';btn.classList.add('ok');const f=btn.closest('.f');f.classList.add('done');state[f.dataset.k]=t;save(state);progress();setTimeout(()=>{btn.textContent='Copy';btn.classList.remove('ok')},1500);};
 if(navigator.clipboard){navigator.clipboard.writeText(t).then(done,()=>{fallback(t);done();});}else{fallback(t);done();}});
progress();
</script></body></html>
`)
	return b.String()
}

func copySheetSlug(title string) string {
	slug := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(title), "-"), "-")
	if slug == "" {
		slug = "copysheet"
	}
	return slug
}

// browserCopySheet handles `bashy browser copysheet FILE|-`.
func browserCopySheet(rc *tool.RunContext, args []string, out string, port int, noOpen, asJSON bool) int {
	if len(args) != 1 {
		fmt.Fprintln(rc.Err, "browser copysheet: want exactly one operand: a JSON file, or - for stdin")
		return 2
	}
	s, err := readCopySheet(rc, args[0])
	if err != nil {
		fmt.Fprintf(rc.Err, "browser copysheet: %v\n", err)
		return 1
	}
	withheld := withhold(s)
	if out == "" {
		out = filepath.Join(copySheetDir(), copySheetSlug(s.Title)+".html")
	} else if !filepath.IsAbs(out) && rc.Dir != "" {
		out = filepath.Join(rc.Dir, out)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(rc.Err, "browser copysheet: %v\n", err)
		return 1
	}
	if err := os.WriteFile(out, []byte(renderCopySheet(s, withheld, false)), 0o600); err != nil {
		fmt.Fprintf(rc.Err, "browser copysheet: %v\n", err)
		return 1
	}
	fields := 0
	for _, sec := range s.Sections {
		fields += len(sec.Fields)
	}
	openedAs := ""
	if !noOpen {
		openedAs = openSheet(port, out)
	}
	opened := openedAs != ""
	if asJSON {
		if withheld == nil {
			withheld = []withheldField{}
		}
		return writeJSON(rc, map[string]any{"success": true, "path": out, "fields": fields, "withheld": withheld, "opened": opened, "opened_as": openedAs})
	}
	fmt.Fprintln(rc.Out, out)
	for _, w := range withheld {
		fmt.Fprintf(rc.Err, "browser copysheet: withheld %q (%s): the human enters it\n", w.Label, w.Reason)
	}
	return 0
}

// copySheetDir is where sheets are written by default. A variable only so
// tests can point it at a temp dir.
var copySheetDir = live.SheetDir

// ExitCopySheetQueued is the exit status of a type/select that bashy could
// not perform because the page refuses scripting, so it queued the value on
// the page's copy sheet for the human to paste.
const ExitCopySheetQueued = 7

// unscriptablePage reports a failure caused by the browser refusing to let
// any extension or script touch the page (the Chrome Web Store, chrome://
// pages, other extensions' pages): retrying cannot help, a human must paste.
func unscriptablePage(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"cannot be scripted", "cannot access a chrome", "cannot access contents of", "chrome-extension://"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// drivenTabURL asks the backend which page the actions run against.
func drivenTabURL(ctx context.Context, c urlReader) string {
	res, err := c.Execute(ctx, wire.Action{Type: wire.ActionTabs})
	if err != nil || res == nil || res.Data == "" {
		return ""
	}
	var tabs []struct {
		URL    string `json:"url"`
		Driven bool   `json:"driven"`
	}
	if json.Unmarshal([]byte(res.Data), &tabs) != nil {
		return ""
	}
	for _, t := range tabs {
		if t.Driven {
			return t.URL
		}
	}
	return ""
}

// queueCopyField is the default fallback for a type/select the page refused:
// the value goes onto that page's copy sheet (opened on its first field and
// refreshing itself after), and the caller learns the human must paste it.
func queueCopyField(rc *tool.RunContext, ctx context.Context, c urlReader, a wire.Action, cause string, port int, noOpen, asJSON bool) (int, bool) {
	if (a.Type != wire.ActionType && a.Type != wire.ActionSelect) || !unscriptablePage(cause) {
		return 0, false
	}
	label := a.Selector
	switch {
	case label == "" && a.MatchText != "":
		label = a.MatchText
	case label == "" && a.ElementID > 0:
		label = fmt.Sprintf("element %d", a.ElementID)
	}
	page := drivenTabURL(ctx, c)
	key := page
	if i := strings.IndexAny(key, "?#"); i >= 0 {
		key = key[:i]
	}
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	if key == "" {
		key = "page"
	}
	dir := copySheetDir()
	base := filepath.Join(dir, "queue-"+copySheetSlug(key))
	var s copySheet
	if b, err := os.ReadFile(base + ".json"); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	fresh := len(s.Fields) == 0
	if fresh {
		s.Title = "Paste into " + key
		s.Intro = "bashy cannot fill this page (the browser does not let extensions script it). Copy each value into the field with the same label on " + page
	}
	replaced := false
	for i := range s.Fields {
		if s.Fields[i].Label == label {
			s.Fields[i].Value, replaced = a.Text, true
		}
	}
	if !replaced {
		s.Fields = append(s.Fields, copyField{Label: label, Value: a.Text})
	}
	// Persist the queue with guarded values already blanked: the secret
	// never reaches disk, not even in the queue file.
	view := copySheet{Title: s.Title, Intro: s.Intro, Sections: []copySection{{Fields: append([]copyField(nil), s.Fields...)}}}
	withheld := withhold(&view)
	s.Fields = view.Sections[0].Fields
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if b, err := json.Marshal(s); err == nil {
			_ = os.WriteFile(base+".json", b, 0o600)
		}
		_ = os.WriteFile(base+".html", []byte(renderCopySheet(&view, withheld, true)), 0o600)
	}
	path := base + ".html"
	if fresh && !noOpen {
		openSheet(port, path)
	}
	held := len(withheld) > 0 && withheld[len(withheld)-1].Label == label
	tell := copySheetTellUser(path)
	if asJSON {
		_ = writeJSON(rc, map[string]any{
			"success": false, "content": "copysheet:queued", "outcome_class": "copysheet_queued",
			"path": path, "url": page, "label": label, "withheld": held, "error": cause,
			"tell_user": tell,
		})
		return ExitCopySheetQueued, true
	}
	fmt.Fprintln(rc.Out, "copysheet:queued")
	fmt.Fprintf(rc.Err, "browser: this page cannot be scripted; queued %q on the copy sheet: %s\n", label, path)
	fmt.Fprintf(rc.Err, "browser: tell the user: %s\n", tell)
	return ExitCopySheetQueued, true
}

// copySheetTellUser is the sentence an agent relays to its human when a
// field was queued instead of filled: the agent does not retry, it hands
// over (the user asked it to fill the form and is waiting).
func copySheetTellUser(path string) string {
	return "I can't fill this page: the browser doesn't let extensions or automation type into it. " +
		"I opened a page with each value ready (" + path + "); keep it beside the form and use its Copy buttons to paste each value into the field with the same label."
}

// openSheetBeside asks the live hub to serve the sheet and the extension to
// open it beside the driven tab (split view on Chrome 155+). It returns
// "split" or "beside"; an error means no hub, no extension, or an extension
// too old for open_beside, and the caller opens the file instead.
func openSheetBeside(port int, path string) (string, error) {
	if port <= 0 {
		return "", fmt.Errorf("no live hub port")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	post := func(endpoint string, body any, out any) error {
		b, _ := json.Marshal(body)
		resp, err := client.Post(base+endpoint, "application/json", bytes.NewReader(b))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", endpoint, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	var reg struct {
		URL string `json:"url"`
	}
	if err := post("/sheet", map[string]string{"path": path}, &reg); err != nil || reg.URL == "" {
		return "", fmt.Errorf("hub does not serve sheets: %v", err)
	}
	var disp struct {
		Result struct {
			Split bool `json:"split"`
		} `json:"result"`
		Error string `json:"error"`
	}
	if err := post("/dispatch", map[string]any{"method": "open_beside", "params": map[string]any{"url": reg.URL}}, &disp); err != nil {
		return "", err
	}
	if disp.Error != "" {
		return "", fmt.Errorf("%s", disp.Error)
	}
	if disp.Result.Split {
		return "split", nil
	}
	return "beside", nil
}

// openSheet opens a written sheet the best way available: beside the driven
// tab through the live hub, else the file in Chrome. It returns how it was
// opened ("split", "beside", "file") or "" when nothing opened.
func openSheet(port int, path string) string {
	if how, err := hooks.openBeside(port, path); err == nil {
		return how
	}
	fileURL := filepath.ToSlash(path)
	if !strings.HasPrefix(fileURL, "/") {
		fileURL = "/" + fileURL // C:/x -> /C:/x
	}
	if hooks.openURL("file://"+fileURL) == nil {
		return "file"
	}
	return ""
}

// browserBeside handles `bashy browser beside FILE.html` (S529.12): any
// agent-made view (a summary, a poster-style digest, a translation, notes)
// opened beside the original page. The file is copied into the sheet dir so
// the live hub may serve it, then opened like a copy sheet: Chrome split
// view next to the driven tab, else the adjacent tab, else the file.
func browserBeside(rc *tool.RunContext, args []string, port int, noOpen, asJSON bool) int {
	if len(args) != 1 {
		fmt.Fprintln(rc.Err, "browser beside: want exactly one operand: an .html file")
		return 2
	}
	src := args[0]
	if !filepath.IsAbs(src) && rc.Dir != "" {
		src = filepath.Join(rc.Dir, src)
	}
	ext := strings.ToLower(filepath.Ext(src))
	if ext != ".html" && ext != ".htm" {
		fmt.Fprintln(rc.Err, "browser beside: the view must be a self-contained .html page")
		return 2
	}
	b, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(rc.Err, "browser beside: %v\n", err)
		return 1
	}
	dir := copySheetDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(rc.Err, "browser beside: %v\n", err)
		return 1
	}
	out := filepath.Join(dir, "view-"+copySheetSlug(strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))+".html")
	if err := os.WriteFile(out, b, 0o600); err != nil {
		fmt.Fprintf(rc.Err, "browser beside: %v\n", err)
		return 1
	}
	openedAs := ""
	if !noOpen {
		openedAs = openSheet(port, out)
	}
	if asJSON {
		return writeJSON(rc, map[string]any{"success": true, "path": out, "opened": openedAs != "", "opened_as": openedAs})
	}
	fmt.Fprintln(rc.Out, out)
	return 0
}
