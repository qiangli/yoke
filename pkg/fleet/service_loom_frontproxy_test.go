package fleet

import (
	"strconv"
	"strings"
	"testing"
)

// S515.1 live round 5 (cloudbox docs/s515-1-live-round5.md): on a satellite,
// `/app/loom/` served Gitea's anonymous Sign In page and never auto-registered
// the signed-in cloud user. The translating front proxy exists
// (external/loom: Remote-User/Remote-Email -> X-WEBAUTH-*), but nothing in the
// request path reached it: `expose.app` named no listener, so the exposure
// reconciler routed the app at the unit's PRIMARY port — Gitea's own
// 31880 — and the supervised argv (`bashy loom serve`) did not start a proxy
// at all. Both halves are unit-spec facts, so both are pinned here.
func TestLoomUnitRoutesTheAppThroughTheFrontProxy(t *testing.T) {
	s, ok := New(serviceOpts(t.TempDir())...).Service("loom")
	if !ok {
		t.Fatal("loom unit missing")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	proxy, ok := s.PortNamed("proxy")
	if !ok {
		t.Fatal("loom declares no proxy listener")
	}
	primary, _ := s.PrimaryPort()
	if primary.Port == proxy.Port {
		t.Fatalf("proxy listener must differ from the gitea listener (both %d)", proxy.Port)
	}

	app := s.Expose.App
	if app == nil {
		t.Fatal("loom declares no app exposure")
	}
	// The whole bug: an empty Port means "the primary listener", i.e. gitea
	// itself, which never translates the identity headers.
	if app.Port != "proxy" {
		t.Fatalf("expose.app.port = %q, want \"proxy\" (gitea must not be the app target)", app.Port)
	}
	if !app.TrustCloudIdentity {
		t.Error("expose.app.trust_cloud_identity must be on: outpost stamps Remote-User only for an app that asked for it, and the front proxy has nothing to translate without it")
	}
	if !app.RequireLogin {
		t.Error("expose.app.require_login must stay on")
	}

	// A mesh peer reaching gitea directly would be reaching a listener with
	// ENABLE_REVERSE_PROXY_AUTHENTICATION on, over a forward that terminates
	// on loopback — i.e. a free X-WEBAUTH-USER login. Mesh goes through the
	// proxy too, which strips client-supplied X-WEBAUTH-* headers.
	if len(s.Expose.Mesh) != 1 || s.Expose.Mesh[0].Name != "git" {
		t.Fatalf("expose.mesh = %+v, want one git service", s.Expose.Mesh)
	}
	if s.Expose.Mesh[0].Port != "proxy" {
		t.Fatalf("expose.mesh[git].port = %q, want \"proxy\"", s.Expose.Mesh[0].Port)
	}

	// The supervised process must serve the proxy itself: a service unit runs
	// ONE foreground command, and `loom start`'s detached proxy child never
	// happens here.
	argv, err := s.ExpandExec("")
	if err != nil {
		t.Fatalf("expand exec: %v", err)
	}
	line := strings.Join(argv, " ")
	if !strings.Contains(line, "--proxy-port "+strconv.Itoa(proxy.EffectivePort())) {
		t.Fatalf("argv = %q, want --proxy-port %d so `loom serve` runs the front proxy", line, proxy.EffectivePort())
	}
}
