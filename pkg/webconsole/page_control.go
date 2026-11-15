package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/coopauth"
)

type pageNamespaces struct{ sync.Map }

func allowPageAction(name string) bool {
	switch name {
	case "shape.open", "shape.draft", "shape.submit", "presentation.shape-only":
		return true
	}
	return false
}

// Live pages are operator capabilities, isolated by authenticated principal and
// server-selected app. Sharees and device sessions cannot borrow a browser's
// authority, even when they can view the underlying app.
func (s *server) handlePageControl(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFromRequest(r)
	if !ok || !s.operatorSession(r) {
		http.Error(w, "page actions require an operator session", http.StatusForbidden)
		return
	}
	if !isSameOrigin(r) {
		http.Error(w, "cross-origin page action refused", http.StatusForbidden)
		return
	}
	if _, ok := s.shapedPanel(r); !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Only cookie-authenticated streams need this watcher. Cloud identity uses
	// the authenticated tunnel; expired cookies must never fall back to OS user.
	if r.PathValue("operation") == "events" && !coopauth.ArrivedViaCloud(r) && s.sessions != nil {
		if cookie, err := r.Cookie(sessionCookie); err == nil {
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if subject, valid := s.sessions.Validate(cookie.Value); !valid || osPrincipal(subject) != principal {
							cancel()
							return
						}
					}
				}
			}()
		}
	}
	cloned := r.Clone(ctx)
	u := *r.URL
	u.Path = "/" + r.PathValue("operation")
	cloned.URL = &u
	if coopauth.ArrivedViaCloud(r) && strings.EqualFold(r.Header.Get("Origin"), coopauth.ExternalBase(r)) {
		if external, err := url.Parse(coopauth.ExternalBase(r)); err == nil {
			cloned.Host = external.Host
		}
	}
	namespace := principal + "\x00" + r.PathValue("app")
	s.pageNamespaces.Store(namespace, principal)
	s.pages.Serve(w, cloned, namespace)
}
