package cdpactions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/qiangli/yoke/pkg/browser/live/extension"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

// Text reads visible text through the page core (page.js text) in the
// guard's isolated world — the same world extract runs in. Over CDP,
// Runtime.evaluate is not subject to the page's Content-Security-Policy,
// so a page that refuses `evaluate` still answers `text`. Raw
// password-input values are suppressed page-side (text() returns the
// field's name only); whole-page reads never contain input values
// (they are not rendered text).
func Text(ctx context.Context, a wire.Action) (*wire.Result, error) {
	// A read resolves through the CURRENT enumeration, not the
	// extract-time snapshot: the fill-mismatch lock-in protects writes
	// from landing in the wrong field, while a read just reads whatever
	// the address names now (like a fresh extract would list it).
	params := map[string]any{"scope": a.Scope, "element_id": a.ElementID, "selector": a.Selector,
		"match_text": a.MatchText, "include_hidden": a.IncludeHidden}
	var out struct {
		Content string `json:"content"`
		Data    string `json:"data"`
		Error   string `json:"error"`
	}
	expr := extension.PageJS + "\n" + fmt.Sprintf("globalThis.__bashyPage.text(%s)", mustJSON(params))
	if err := evalGuard(ctx, expr, &out); err != nil {
		return &wire.Result{Error: err.Error()}, nil
	}
	if out.Error != "" {
		return &wire.Result{Error: out.Error}, nil
	}
	res := &wire.Result{Success: true, Content: out.Content}
	if out.Data != "" {
		res.Data = out.Data
	}
	return res, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
