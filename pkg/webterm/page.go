// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webterm

import (
	"net/http"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// servePage serves the terminal panel's own page.
//
// It is deliberately thin. The renderer lives in the LAUNCHER's SPA (which
// already vendors xterm.js and can host the terminal inside one nav and one
// auth), so duplicating it here would mean vendoring xterm a second time or
// reaching into the launcher's asset layout — coupling this package to its
// current consumer. What this package owns is the socket; the page just points
// a direct visitor at the UI.
func servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page(coopauth.BaseHref(r), Supported())))
}

func page(base string, supported bool) string {
	body := `<p>This is the terminal's socket endpoint, not its screen. The
      terminal opens inside the launcher.</p>
    <p><a id="go" href="../#/terminal">Open the Terminal &rarr;</a></p>
    <p>The socket itself is <code id="ws"></code> — binary frames carry raw PTY
      bytes, and a text frame <code>{"type":"size","cols":N,"rows":N}</code>
      resizes it.</p>`
	if !supported {
		body = `<p>This host has no pseudo-console, so the terminal cannot run here.</p>`
	}
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<base href="` + base + `">
<title>Terminal &mdash; bashy apps</title>
<style>
:root{--bg:#f7f7f8;--fg:#18181b;--muted:#71717a}
@media (prefers-color-scheme:dark){:root{--bg:#09090b;--fg:#fafafa;--muted:#a1a1aa}}
body{margin:0;background:var(--bg);color:var(--fg);
 font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:44rem;margin:0 auto;padding:3rem 1.5rem}
h1{font-size:1.2rem;margin:0 0 .5rem}
p{color:var(--muted)}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.85rem;
 background:rgba(127,127,127,.14);padding:.1rem .35rem;border-radius:.3rem;color:var(--fg)}
a{color:inherit}
</style></head>
<body><main>
<h1>Terminal</h1>
` + body + `
</main>
<script>
const el = document.getElementById("ws");
if (el) {
  const u = new URL("ws", document.baseURI);
  u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
  el.textContent = u.toString();
}
</script>
</body></html>
`
}
