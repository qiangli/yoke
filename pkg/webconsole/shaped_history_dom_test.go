//go:build verifydom

package webconsole

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestDOMShapeSimpleHistory(t *testing.T) {
	const old = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const current = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	active := current
	panel := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: newNotesSource(t), Auth: AuthSystem, Source: "registered", Available: true}
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/shape/notes":
				_ = json.NewEncoder(w).Encode(map[string]any{"principal": "alice", "active": active,
					"candidates": []any{map[string]any{"id": "ready1", "request": "Make notes easier to scan", "status": "ready", "rev": current}}})
				return
			case "/api/shape/notes/history":
				_ = json.NewEncoder(w).Encode(map[string]any{"active": active, "versions": []any{
					map[string]any{"revision": current, "activation": current, "summary": "Newer saved wish", "author": "Bob", "when": "2026-10-31T10:00:00Z", "active": active == current},
					map[string]any{"revision": old, "activation": old, "summary": "Older saved wish", "author": "Alice", "when": "2026-10-30T10:00:00Z", "active": active == old},
				}})
				return
			case "/api/shape/notes/history/restore":
				active = old
				_ = json.NewEncoder(w).Encode(map[string]string{"active": active})
				return
			case "/api/shape/notes/history/undo":
				active = current
				_ = json.NewEncoder(w).Encode(map[string]string{"active": active})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	base, ctx, errors := domEnvWrapped(t, Options{Panels: []Panel{panel}}, wrap)
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/shape/notes/"), chromedp.WaitVisible(".history-version", chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Text    string `json:"text"`
		Link    string `json:"link"`
		Restore bool   `json:"restore"`
	}
	read := func() {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Evaluate(`Array.from(document.querySelectorAll('.history-version')).map(r => ({text:r.innerText, link:r.querySelector('a.history-details')?.pathname, restore:!!r.querySelector('button[aria-label^="Restore"]')}))`, &rows)); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if len(rows) != 2 || !strings.Contains(rows[0].Text, "Newer saved wish") || !strings.Contains(rows[0].Text, "Bob") || !strings.Contains(rows[0].Text, "2026") || !strings.Contains(rows[0].Text, "Current") || !rows[0].Restore {
		t.Fatalf("history rows = %+v", rows)
	}
	if !strings.Contains(rows[1].Text, "Older saved wish") || !strings.Contains(rows[1].Text, "Alice") || rows[1].Link != "/loom/qiangli/notes/commit/"+old {
		t.Fatalf("older row = %+v", rows[1])
	}
	var simple bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`!document.querySelector('#shape-page pre, #shape-page code, #shape-page .shape-diff, #history-diff-host, #history-compare') && !document.querySelector('#shape-page').innerText.includes('Code diff') && !!document.querySelector('#shape-list a[href*="/preview/"]') && ['Approve','Reject'].every(x => Array.from(document.querySelectorAll('#shape-list button')).some(b => b.innerText === x))`, &simple)); err != nil {
		t.Fatal(err)
	}
	if !simple {
		t.Fatal("shape page includes code/diff or lacks pending review actions")
	}
	if err := chromedp.Run(ctx, chromedp.Click(`.history-version:nth-child(2) button`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		read()
		if strings.Contains(rows[1].Text, "Current") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restore did not mark older version Current: %+v", rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := chromedp.Run(ctx, chromedp.Click(`#history-undo`, chromedp.ByQuery)); err != nil {
		t.Fatal(err)
	}
	for {
		read()
		if strings.Contains(rows[0].Text, "Current") {
			break
		}
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatalf("undo did not mark newer version Current: %+v", rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
	assertNoJSErrors(t, "simple history", errors())
}
