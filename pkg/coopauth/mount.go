// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coopauth

import (
	"net/http"
	"strings"
)

// trustHeaders are the cooperative-auth headers a caller must never be able to
// set for themselves. They are stamped by outpost on the loopback hop.
var trustHeaders = []string{
	HdrForwardedPrefix,
	HdrForwardedHost,
	HdrForwardedProto,
	HdrRemoteUser,
	HdrRemoteEmail,
	HdrRemoteName,
	HdrRemoteGroups,
	HdrIdentityTs,
	HdrIdentitySig,
}

// StripSpoofedTrustHeaders deletes the cooperative-auth headers from any request
// whose PEER is not loopback.
//
// The vouch model assumes outpost is the only thing that stamps Remote-*, and
// outpost always dials a cooperative app over 127.0.0.1 — so a loopback peer is
// the only way a vouched request can legitimately arrive. Without this, an app
// bound to a non-loopback address is open to anyone who sends
// `X-Forwarded-Prefix: /x` plus `Remote-User: root`: Guard.verify admits on
// ArrivedViaCloud, and RequireHMAC is off whenever no SSO secret is configured,
// which is the default.
//
// Note this strips on the PEER address (r.RemoteAddr), never on X-Forwarded-For
// — the whole point is that forwarded headers are what we distrust here.
func StripSpoofedTrustHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsLoopbackAddr(r.RemoteAddr) {
			for _, h := range trustHeaders {
				r.Header.Del(h)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Mount serves next at prefix beneath the caller's OWN mount: it strips prefix
// from the request path and rewrites X-Forwarded-Prefix to <incoming>+prefix, so
// a nested cooperative app renders the right <base href> whether its host was
// reached on loopback or behind outpost's tunnel.
//
// Composition is by CONCATENATION, never replacement. For a console mounted at
// /matrix/h/laptop/app/console that mounts a room at /relay:
//
//	outpost -> console:  path /meet/api/rooms  prefix /matrix/h/laptop/app/console
//	console -> room:     path /api/rooms        prefix /matrix/h/laptop/app/console/relay
//
// The trailing slash on the resulting <base href> is the entire mechanism (see
// BaseHref) — an SPA that builds URLs as new URL(x, document.baseURI) then needs
// no per-mount configuration.
//
// TRAP: setting X-Forwarded-Prefix makes ArrivedViaCloud(r) report true. A
// nested app whose gate reads that header as "arrived through the tunnel" will
// demand a vouched identity and 403 the machine owner on 127.0.0.1. A mountable
// app must therefore let its host inject a gate (see meet.MountOptions) — the
// host has already decided who the caller is by the time it delegates.
func Mount(prefix string, next http.Handler) http.Handler {
	prefix = "/" + strings.Trim(prefix, "/")
	if prefix == "/" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if rest == "" {
			// /meet -> /meet/ so the SPA's <base href> keeps its slash.
			http.Redirect(w, r, PrefixPath(r, prefix+"/"), http.StatusMovedPermanently)
			return
		}
		if !strings.HasPrefix(rest, "/") {
			// A sibling that merely shares our prefix as a substring.
			next.ServeHTTP(w, r)
			return
		}

		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		if r.URL.RawPath != "" {
			r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, prefix)
		}
		r2.Header.Set(HdrForwardedPrefix, BasePrefix(r)+prefix)
		next.ServeHTTP(w, r2)
	})
}
