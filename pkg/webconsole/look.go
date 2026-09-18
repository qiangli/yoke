// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The console's global look-and-feel settings — the one knob of which the
// "Open apps" mode is the first member.
//
// This is SERVER-persisted, not localStorage, because the mode is GLOBAL in
// the strict sense: it decides how the SERVER injects chrome (the managed
// apps' return control exists only in same-tab mode), so the server has to
// know it at serve time — a per-browser copy could not, and the two halves of
// the setting would drift apart the moment two browsers disagreed. One file,
// one truth, every viewer sees the same console.
const (
	// lookSchema identifies the persisted settings document.
	lookSchema = "bashy-console-look-v1"

	// OpenSameTab navigates the current window: the launcher is replaced by
	// the app, and a managed app carries the return control that walks back.
	// It is the DEFAULT because it is the safe default — the whole console
	// stays one readable history, and nothing about a tile click can surprise
	// a reader who did not ask for new windows.
	OpenSameTab = "same-tab"
	// OpenNewTab gives each app its own tab (target=_blank, rel=noopener) and
	// omits the return control, since the launcher is still right there under
	// the tab it came from.
	OpenNewTab = "new-tab"
)

// lookState is the persisted document. Unknown fields are ignored on load so
// a newer console's file degrades instead of poisoning an older one.
type lookState struct {
	Schema   string `json:"schema_version"`
	OpenApps string `json:"open_apps"`
}

// defaults is the document a fresh (or unreadable) store serves.
func defaultLook() lookState {
	return lookState{Schema: lookSchema, OpenApps: OpenSameTab}
}

// validOpenApps reports whether v is one of the two documented modes. There
// are exactly two states, never a near-miss: anything else fails loudly.
func validOpenApps(v string) bool {
	return v == OpenSameTab || v == OpenNewTab
}

// lookStore is the file-backed settings state, safe for one process. It
// follows pairStore's shape: no I/O at construction, reads are cached on
// mtime+size so an external edit is still picked up, and every write is
// atomic (temp file + rename) so a crash can never leave a torn document.
type lookStore struct {
	path string

	mu     sync.Mutex
	cached lookState
	loaded bool
	mod    time.Time
	size   int64
}

// lookPath is the settings document's location: the same ladder as the
// pairing document, so relocating $BASHY_HOME relocates console state with
// it — and so a test that does want persistence can point it somewhere
// disposable.
func lookPath() (string, error) {
	dir, err := serviceDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ui.json"), nil
}

// newLookStore builds a store. It performs no I/O: constructing one must
// never be the reason a console fails to boot.
func newLookStore(path string) *lookStore {
	return &lookStore{path: path}
}

// openApps returns the current mode. A store whose file is missing, unreadable
// or corrupt serves the DEFAULT rather than an error: the console's chrome
// must render even when its preferences do not, and the default is the safe
// mode. The failure is logged — degraded is not silent.
func (s *lookStore) openApps() string {
	if s == nil {
		return OpenSameTab
	}
	st, err := s.load()
	if err != nil {
		slog.Warn("apps: console look store unreadable; serving defaults", "path", s.path, "err", err)
		return OpenSameTab
	}
	if !validOpenApps(st.OpenApps) {
		// A hand-edited file can hold anything. Same rule as a corrupt one:
		// serve the default, say so, keep booting.
		slog.Warn("apps: console look store has unknown open_apps; serving default", "path", s.path, "value", st.OpenApps)
		return OpenSameTab
	}
	return st.OpenApps
}

// load returns the current state, re-reading only when the file changed.
func (s *lookStore) load() (lookState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultLook(), nil
		}
		return defaultLook(), err
	}
	if s.loaded && fi.ModTime().Equal(s.mod) && fi.Size() == s.size {
		return s.cached, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return defaultLook(), err
	}
	st := defaultLook()
	if err := json.Unmarshal(raw, &st); err != nil {
		return defaultLook(), fmt.Errorf("look store %s is unreadable: %w", s.path, err)
	}
	s.cached, s.loaded, s.mod, s.size = st, true, fi.ModTime(), fi.Size()
	return st, nil
}

// setOpenApps validates and persists one mode, atomically, and returns the
// stored state. An unknown value is a caller error, not a store failure.
func (s *lookStore) setOpenApps(v string) (lookState, error) {
	if !validOpenApps(v) {
		return defaultLook(), fmt.Errorf("open_apps must be %q or %q, got %q", OpenSameTab, OpenNewTab, v)
	}
	st := defaultLook()
	st.OpenApps = v
	if err := s.write(st); err != nil {
		return defaultLook(), err
	}
	return st, nil
}

// write replaces the document: a temp file in the SAME directory (so rename
// stays atomic on one filesystem), then rename over the target. A reader —
// this process or another — sees either the old bytes or the new ones, never
// half of either.
func (s *lookStore) write(st lookState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	if fi, err := os.Stat(s.path); err == nil {
		s.cached, s.loaded, s.mod, s.size = st, true, fi.ModTime(), fi.Size()
	}
	return nil
}

// openAppsMode is the server's nil-safe accessor: a console built without a
// store (or whose store failed to open) still renders, in the default mode.
func (s *server) openAppsMode() string {
	if s == nil || s.look == nil {
		return OpenSameTab
	}
	return s.look.openApps()
}

// handleLookGet serves the structured projection of the settings: the same
// JSON an embedder or a CLI (`bashy app look`) would read, not markup.
func (s *server) handleLookGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, lookView(s.openAppsMode()))
}

// handleLookPut sets one mode. It takes JSON and answers JSON so the surface
// stays composable; the DIALOG in the launcher is the human surface and it
// speaks to this one.
//
// Unknown values are a 400 that names the value and the two valid ones — a
// near-miss ("newtab", "new_tab") must fail loudly, never round to a mode.
func (s *server) handleLookPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OpenApps string `json:"open_apps"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "look: invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !validOpenApps(body.OpenApps) {
		http.Error(w, fmt.Sprintf("look: open_apps must be %q or %q, got %q",
			OpenSameTab, OpenNewTab, body.OpenApps), http.StatusBadRequest)
		return
	}
	if s.look == nil {
		// No store configured (a test shape in practice): accept the value
		// for this process rather than pretending a write that cannot land.
		s.look = newLookStore("")
		s.look.cached, s.look.loaded = lookState{Schema: lookSchema, OpenApps: body.OpenApps}, true
		writeJSON(w, http.StatusOK, lookView(body.OpenApps))
		return
	}
	st, err := s.look.setOpenApps(body.OpenApps)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			http.Error(w, "look: settings file is not writable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Error(w, "look: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, lookView(st.OpenApps))
}

// lookView is the wire shape of the settings: versioned, machine-readable,
// stable field names — the composable half of a human-first surface.
func lookView(openApps string) map[string]any {
	return map[string]any{
		"schema_version": lookSchema,
		"open_apps":      openApps,
	}
}
