// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/qiangli/yoke/pkg/coopauth"
)

// PrincipalHeader is the launcher-to-app identity header. It is never accepted
// from an inbound request; the launcher deletes it and sets a fresh value only
// after its own authentication gate resolves exactly one principal.
const PrincipalHeader = "X-Bashy-Principal"

// HeaderPrincipal is kept as a descriptive alias for integrations.
const HeaderPrincipal = PrincipalHeader

type principalContextKey struct{}

// PrincipalFromRequest returns the authenticated web principal established by
// the launcher gate. false means base-only behavior: no per-user shaping state
// or shaping verbs may be selected.
func PrincipalFromRequest(r *http.Request) (string, bool) {
	p, ok := r.Context().Value(principalContextKey{}).(string)
	return p, ok && p != ""
}

func withPrincipal(r *http.Request, principal string) *http.Request {
	if principal == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
}

func cloudPrincipal(r *http.Request) string {
	id, ok := coopauth.IdentityOf(r)
	if !ok || !id.Authenticated() {
		return ""
	}
	user := strings.TrimSpace(r.Header.Get(coopauth.HdrRemoteUser))
	email := strings.TrimSpace(r.Header.Get(coopauth.HdrRemoteEmail))
	if user != "" && email != "" && !strings.EqualFold(user, email) {
		return "" // two different vouched identities is ambiguous
	}
	canonical := strings.TrimSpace(id.User)
	if canonical == "" {
		canonical = strings.TrimSpace(id.Email)
	}
	if canonical == "" {
		return ""
	}
	return "cloud:" + strings.ToLower(canonical)
}

func osPrincipal(subject string) string {
	owner := strings.TrimSpace(currentOSUser())
	if owner == "" || !strings.EqualFold(strings.TrimSpace(subject), owner) {
		return ""
	}
	return "os:" + owner
}

func devicePrincipal(name string) string {
	if validatePrincipalName(name) != nil {
		return ""
	}
	return "device:" + strings.TrimSpace(name)
}

func validatePrincipalName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("principal name is empty")
	}
	if len(name) > 128 || strings.ContainsAny(name, "/\\:") || name == "." || name == ".." {
		return errors.New("principal name must be 1-128 characters without slash, backslash, or colon")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("principal name contains a control character")
		}
	}
	return nil
}

func stripInboundPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(PrincipalHeader)
		next.ServeHTTP(w, r)
	})
}
