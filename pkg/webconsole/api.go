// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// appsSchemaVersion identifies the /api/apps payload. Bump it only for a
// breaking shape change; the SPA reads it and can then say so.
const appsSchemaVersion = "bashy-console-apps-v1"

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": otelServiceName})
}

// handleApps is the tile list: what exists, whether it is up right now, and the
// command that would start it if it is not. It is the same data
// `bashy commands --view web` prints — one source, two renderers.
func (s *server) handleApps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": appsSchemaVersion,
		"base":           coopauth.BaseHref(r),
		"apps":           s.probes.Probe(r.Context(), s.panels),
	})
}

// handleSession tells the SPA who it is talking as, so it can render an identity
// without guessing from the presence of a cookie.
func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	user, via := s.userOf(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":          user,
		"via":           via,
		"require_login": s.requireLogin,
		// Whether this console is armed for phone pairing. The Settings page
		// reads it to tell "enabling will mint a code" from "enabling will show
		// you the restart command" — without minting anything to find out.
		"pairing": s.pairing != nil,
		"build":   BuildOf(),
	})
}

// userOf answers who this request is, by the console's own gate ladder: the
// tunnel's vouched identity, else a console session cookie, else the OS user
// this process runs as.
//
// It is one function because a SECOND surface now signs with it. The message
// board's one guarantee is that a post names who sent it, so the name the header
// shows and the name a post is signed with must be the same name, derived once —
// a browser never gets to supply either.
func (s *server) userOf(r *http.Request) (user, via string) {
	via = "loopback"
	if coopauth.ArrivedViaCloud(r) {
		via = "cloud"
		if id, ok := coopauth.IdentityOf(r); ok {
			user = id.Username
		}
	} else if s.sessions != nil {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if u, ok := s.sessions.Validate(c.Value); ok {
				user, via = u, "session"
			}
		}
	}
	if user == "" {
		user = coopauth.SystemUser()
	}
	return user, via
}

// MetaView is the EXTERNAL projection of a panel — the third surface of the
// dhnt-app-meta-v1 contract, beside `<bin> meta` and `bashy <verb> meta`.
//
// It deliberately omits port, start and status. The port and the start argv are
// this host's internals and mean nothing to a remote renderer; liveness changes
// constantly and is exactly what would make the response uncacheable. Liveness
// stays on /api/apps, which the launcher already polls behind a 3s TTL.
type MetaView struct {
	SchemaVersion string `json:"schema_version"`
	Name          string `json:"name"`
	Label         string `json:"label"`
	Icon          string `json:"icon,omitempty"`
	Tip           string `json:"tip,omitempty"`
	Mount         string `json:"mount"`
	Path          string `json:"path"`
	Auth          string `json:"auth"`
	Source        string `json:"source"`
}

func metaViewOf(p Panel) MetaView {
	auth := p.Auth
	if auth == "" {
		auth = AuthSystem
	}
	return MetaView{
		SchemaVersion: MetaSchema,
		Name:          p.Name,
		Label:         p.Label,
		Icon:          p.Icon,
		Tip:           p.Tip,
		Mount:         strings.Trim(p.Path, "/"),
		Path:          p.Path,
		Auth:          auth,
		Source:        p.Source,
	}
}

// writeMetaJSON serves a body with a strong content-derived ETag and honours
// If-None-Match. The point of this endpoint is that outpost or cloudbox can poll
// it cheaply and cache until the bytes actually change, so the validator is
// keyed on content — the same choice embed.go's etagFor makes for assets, and
// for the same reason.
func writeMetaJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "meta: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// handleMeta lists every panel this console serves.
func (s *server) handleMeta(w http.ResponseWriter, r *http.Request) {
	views := make([]MetaView, 0, len(s.panels))
	for _, p := range s.panels {
		views = append(views, metaViewOf(p))
	}
	writeMetaJSON(w, r, map[string]any{
		"schema_version": MetaSchema,
		"apps":           views,
	})
}

// handleMetaApp answers for one panel.
//
// A panel removed by --disable is 404 here, exactly as it is absent from
// /api/apps and unrouted. Reporting it would leak the existence of a surface
// that --disable exists to remove.
func (s *server) handleMetaApp(w http.ResponseWriter, r *http.Request) {
	want := r.PathValue("app")
	for _, p := range s.panels {
		if p.Name == want || strings.Trim(p.Path, "/") == want {
			writeMetaJSON(w, r, metaViewOf(p))
			return
		}
	}
	http.Error(w, "no such app: "+want, http.StatusNotFound)
}
