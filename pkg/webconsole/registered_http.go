// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/qiangli/yoke/pkg/fleet"
)

type registeredPanelsKey struct{}

// Fixed panels remain immutable. Registered records are read as data per
// request; the gate and dispatcher use the same snapshot of auth and port.
func (s *server) currentPanels() []Panel {
	if !s.liveRegistered {
		return s.panels
	}
	panels := append([]Panel(nil), s.panels...)
	taken := TakenMounts(stockPanels())
	for name := range TakenMounts(s.panels) {
		taken[name] = true
	}
	registered, errs := discoverRegistered(taken)
	for _, err := range errs {
		slog.Error("apps: skipping registered app", "err", err)
	}
	for _, p := range registered {
		if !s.opts.disabled(p) {
			panels = append(panels, p)
		}
	}
	return panels
}

func (s *server) requestPanels(r *http.Request) []Panel {
	if panels, ok := r.Context().Value(registeredPanelsKey{}).([]Panel); ok {
		return panels
	}
	return s.currentPanels()
}

func (s *server) registeredCatalog() *fleet.Catalog {
	return fleet.New(fleet.WithAppValidate(func(a fleet.App) error {
		if err := ValidateRegisteredApp(a); err != nil {
			return err
		}
		if TakenMounts(s.panels)[a.Name] {
			return fmt.Errorf("mount %q is already claimed by another panel", a.Name)
		}
		return nil
	}))
}

func (s *server) registryWriter(w http.ResponseWriter, r *http.Request) bool {
	user, _ := s.userOf(r)
	if _, _, device := splitDeviceSubject(user); device {
		http.Error(w, "registered apps require an operator session", http.StatusForbidden)
		return false
	}
	return true
}

func (s *server) handleRegisteredSave(w http.ResponseWriter, r *http.Request) {
	if !s.registryWriter(w, r) {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "registered apps require application/json", http.StatusUnsupportedMediaType)
		return
	}
	var app fleet.App
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&app); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected one app record", http.StatusBadRequest)
		return
	}
	if err := s.registeredCatalog().SaveApp(app); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": app.Name})
}

func (s *server) handleRegisteredRemove(w http.ResponseWriter, r *http.Request) {
	if !s.registryWriter(w, r) {
		return
	}
	name := r.PathValue("name")
	if err := validMount(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.registeredCatalog().RemoveApp(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
