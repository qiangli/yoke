package pagecontrol

import (
	_ "embed"
	"net/http"
)

// Script is shared as-is by each application; URLs and identity are supplied
// by its adapter rather than hard-coded to Cloudbox or a browser framework.
//
//go:embed page.js
var Script []byte

func ServeScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(Script)
}
