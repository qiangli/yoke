// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas

import "sort"

// How the console reaches a declared surface (closed vocabulary).
const (
	// WebSelf is the console itself.
	WebSelf = "self"
	// WebInProcess means the console mounts the command's own http.Handler —
	// one implementation, two hosts. Available iff the command is in the binary.
	WebInProcess = "in-process"
	// WebProxy means a separately-supervised service on a loopback port that the
	// console reverse-proxies. Its lifecycle belongs to that service, not to the
	// console.
	WebProxy = "proxy"
)

// WebSurface declares that a command serves a browser UI, and how the console
// reaches it.
//
// PRESENCE IS THE DECLARATION. There is deliberately no companion capability
// flag: two places to say the same thing is two places to drift, and the
// console needs the path and port anyway, which a bare flag cannot carry.
type WebSurface struct {
	Label string   `json:"label"`           // tile title: "Meet", "Loom"
	Mount string   `json:"mount"`           // ONE path segment, no slashes; "" for WebSelf
	Mode  string   `json:"mode"`            // WebSelf | WebInProcess | WebProxy
	Port  int      `json:"port,omitempty"`  // the service's loopback port
	Start []string `json:"start,omitempty"` // argv AFTER `bashy` — the start hint

	// Icon is SVG path data on a 24 grid, or a single emoji. Optional: the
	// launcher falls back to its own mark for a known name, then to the
	// initial. It lives here rather than only in the launcher's script so a
	// built-in surface and a third-party app describe themselves through ONE
	// struct — see pkg/webconsole's AppMeta, which this round-trips through.
	Icon string `json:"icon,omitempty"`
	// Tip is the tile's title= tooltip. Optional; absent means no tooltip.
	Tip string `json:"tip,omitempty"`

	DefaultOn bool `json:"default_on,omitempty"`
}

// WebModes returns the closed mode vocabulary, sorted.
func WebModes() []string { return []string{WebInProcess, WebProxy, WebSelf} }

// WebSurfaces returns every verb that declares a browser UI, keyed by verb name.
func WebSurfaces() map[string]WebSurface {
	out := map[string]WebSurface{}
	for name, e := range verbs {
		if e.Web != nil {
			out[name] = *e.Web
		}
	}
	return out
}

// WebSurfaceNames returns the declaring verb names, sorted.
func WebSurfaceNames() []string {
	out := make([]string, 0, len(verbs))
	for name, e := range verbs {
		if e.Web != nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// reservedMounts mirrors outpost's admincore reservedNames plus the cloudbox
// top-level namespace it is itself kept in lockstep with (outpost
// internal/agent/admincore/validate.go, cloudbox hub/internal/reserved). A
// surface whose mount could never be published as a cooperative app must not be
// declarable in the first place — catching it here, at init, is cheaper than
// catching it when someone tries to pair the host.
//
// Keep this to the NAME list only. It is a third copy of a list that already
// exists twice; every field it does not duplicate is one that cannot drift.
var reservedMounts = map[string]bool{
	"api": true, "static": true, "healthz": true, "index.html": true,
	"app": true, "mcp": true, "_periscope": true,
	"cloudbox": true, "periscope": true, "matrix": true, "cloud": true,
	"v1": true, "health": true, "version": true, "metrics": true,
	"config": true, "overlay": true, "embed.js": true, "favicon.ico": true,
	".well-known": true,
}

// ReservedMounts returns the mount names a web surface may not claim, sorted.
func ReservedMounts() []string {
	out := make([]string, 0, len(reservedMounts))
	for n := range reservedMounts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func validWebMode(m string) bool {
	for _, v := range WebModes() {
		if m == v {
			return true
		}
	}
	return false
}
