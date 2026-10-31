// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package appserver reads the one file a shaped app adds to declare a
// long-running server of its own: shaped.json at the app root.
//
//	{"server": {"main": ".", "args": ["serve", "-addr", "$ADDR", "-data", "$DATA"]}}
//
// main is the Go main package, relative to the app root (default "."). The
// console builds it from the principal's active revision with a fixed
// command — `go build -trimpath -o BIN ./MAIN` under GOWORK=off and
// CGO_ENABLED=0 — and starts BIN with args, expanding $ADDR (the loopback
// host:port the console chose), $PORT and $DATA (the principal's app data
// directory). The build command is deliberately NOT part of the manifest:
// the manifest is shaped source, and a shaping agent must not be able to
// turn a revision into an arbitrary command the console runs at build time.
//
// An app without shaped.json, or whose shaped.json has no server, is served
// exactly as before (public/ assets and api/<name>.md units).
//
// The same file carries the app's placement of the common Genie button
// (Sprint 526.B1g) — the protected component the console injects into every
// shaped app page:
//
//	{"genie": {"position": "bottom-left", "hidden": true, "anchor": "#toolbar"}}
//
// position is a corner (top-right by default); anchor is an optional CSS
// selector list the button is placed inside when one matches; hidden keeps
// the button off every page but the app's landing page (the keyboard
// shortcut still opens the wish panel). Placement is shaped source — a wish
// to move or hide the button is a config-only candidate — while the
// component itself stays protected.
package appserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// File is the manifest's name at the app root.
const File = "shaped.json"

// Manifest is shaped.json.
type Manifest struct {
	Server *Server `json:"server,omitempty"`
	// Tidy sets the support window and optionally names an api unit that
	// cleans this app's own principal data before history is compacted.
	Tidy *Tidy `json:"tidy,omitempty"`
	// Properties are the app's declared stored properties. The trusted
	// verifier refuses a candidate that removes or renames one.
	Properties []string `json:"properties,omitempty"`
	// Genie places the common Genie button on this app's pages.
	Genie *Genie `json:"genie,omitempty"`
}

// Genie is the app's placement of the protected Genie button.
type Genie struct {
	// Position is the corner the button sits in: top-right (the default),
	// top-left, bottom-right or bottom-left.
	Position string `json:"position,omitempty"`
	// Anchor is a comma-separated CSS selector list; the button is placed
	// inside the first element that matches, instead of a corner.
	Anchor string `json:"anchor,omitempty"`
	// Hidden keeps the button off every page except the app's landing page.
	Hidden bool `json:"hidden,omitempty"`
}

// GeniePositions are the corners a Genie placement may name.
var GeniePositions = []string{"top-right", "top-left", "bottom-right", "bottom-left"}

// GenieDefaultPosition is the corner used when the manifest names none.
const GenieDefaultPosition = "top-right"

type Tidy struct {
	KeepLast int    `json:"keep_last,omitempty"`
	KeepDays int    `json:"keep_days,omitempty"`
	Unit     string `json:"unit,omitempty"`
}

// Server is the app's declared server.
type Server struct {
	// Main is the Go main package directory, slash-separated and relative
	// to the app root. Empty = ".".
	Main string `json:"main,omitempty"`
	// Args is the server's argv after the binary. It must name $ADDR or
	// $PORT: a server that is not told where to listen cannot be reached.
	Args []string `json:"args"`
}

// Parse decodes and validates a manifest. Unknown fields are an error so a
// misspelled key fails loudly instead of being served as a manifest-less app.
func Parse(b []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", File, err)
	}
	if dec.More() {
		return Manifest{}, fmt.Errorf("%s: trailing data after the manifest", File)
	}
	if m.Tidy != nil {
		if m.Tidy.KeepLast < 0 || m.Tidy.KeepDays < 0 {
			return Manifest{}, fmt.Errorf("%s: tidy retention cannot be negative", File)
		}
		name := m.Tidy.Unit
		if name != "" {
			if name == "." || strings.ContainsAny(name, `/\\`) || strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
				return Manifest{}, fmt.Errorf("%s: tidy.unit must be a single unit name", File)
			}
		}
	}
	if m.Genie != nil {
		if m.Genie.Position == "" {
			m.Genie.Position = GenieDefaultPosition
		}
		if !slices.Contains(GeniePositions, m.Genie.Position) {
			return Manifest{}, fmt.Errorf("%s: genie.position %q must be one of %s", File, m.Genie.Position, strings.Join(GeniePositions, ", "))
		}
		if strings.ContainsAny(m.Genie.Anchor, "<>\"'") || len(m.Genie.Anchor) > 200 {
			return Manifest{}, fmt.Errorf("%s: genie.anchor must be a short CSS selector list", File)
		}
	}
	if m.Server == nil {
		return m, nil
	}
	main, err := cleanMain(m.Server.Main)
	if err != nil {
		return Manifest{}, err
	}
	m.Server.Main = main
	listens := false
	for _, a := range m.Server.Args {
		if strings.Contains(a, "$ADDR") || strings.Contains(a, "$PORT") {
			listens = true
		}
	}
	if !listens {
		return Manifest{}, fmt.Errorf("%s: server.args must pass $ADDR or $PORT so the server listens where the console proxies", File)
	}
	return m, nil
}

func cleanMain(m string) (string, error) {
	if m == "" {
		return ".", nil
	}
	if strings.Contains(m, `\`) || path.IsAbs(m) {
		return "", fmt.Errorf("%s: server.main %q must be a relative slash path inside the app", File, m)
	}
	c := path.Clean(m)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%s: server.main %q leaves the app", File, m)
	}
	return c, nil
}

// Argv expands the server's args for one run: addr is the loopback
// host:port the server must listen on, data its private data directory.
func (s Server) Argv(addr string, port int, data string) []string {
	r := strings.NewReplacer("$ADDR", addr, "$PORT", strconv.Itoa(port), "$DATA", data)
	out := make([]string, len(s.Args))
	for i, a := range s.Args {
		out[i] = r.Replace(a)
	}
	return out
}
