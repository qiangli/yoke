package provider

import (
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

func TestTierOfSixTiers(t *testing.T) {
	for _, c := range []struct {
		loc       ref.Locator
		tier      string
		transport string
	}{
		{ref.Locator{Scheme: "dir", Target: "x"}, "local", "local"},
		{ref.Locator{Scheme: "git", Target: "x", Revision: "HEAD"}, "local", "local"},
		{ref.Locator{Scheme: "tar", Target: "a.tar"}, "local", "local"},
		{ref.Locator{Scheme: "ast", Target: "."}, "local", "local"},
		{ref.Locator{Scheme: "proc", Target: "pids"}, "local", "local"},
		{ref.Locator{Scheme: "env", Target: "."}, "local", "local"},
		{ref.Locator{Scheme: "oci", Target: "containers"}, "local", "podman"},
		{ref.Locator{Scheme: "sphere", Target: ""}, "sphere", "spacegraph"},
		{ref.Locator{Scheme: "dir", Target: ".", ScopeKind: "ws", ScopeValue: "q"}, "workspace", "local"},
		{ref.Locator{Scheme: "dir", Target: ".", ScopeKind: "sandbox", ScopeValue: "q"}, "sandbox", "local"},
		{ref.Locator{Scheme: "dir", Target: ".", ScopeKind: "host", ScopeValue: "peer"}, "sphere", "outpost-sftp"},
		{ref.Locator{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "ctx"}, "k8s", "kubectl"},
		{ref.Locator{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "ctx"}, "cloud", "doctl"},
		{ref.Locator{Scheme: "k8s", Target: "pods"}, "k8s", "kubectl"},
		{ref.Locator{Scheme: "cloud", Target: "droplets"}, "cloud", "doctl"},
		{ref.Locator{Scheme: "nope", Target: "x"}, "local", "local"},
		{ref.Locator{Scheme: "dir", Target: ".", ScopeKind: "nope", ScopeValue: "x"}, "unknown", "unknown"},
	} {
		if got := TierOf(c.loc); got != c.tier {
			t.Errorf("%v: tier=%q want %q", c.loc, got, c.tier)
		}
		if got := TransportOf(c.loc); got != c.transport {
			t.Errorf("%v: transport=%q want %q", c.loc, got, c.transport)
		}
	}
}

// TestTierReadsNoEnvironment pins the credential rule: tier and transport
// derive from the locator alone, so secret-bearing variables in scope kinds
// the grammar refuses (or anywhere else) can never surface in the names.
func TestTierReadsNoEnvironment(t *testing.T) {
	secrets := map[string]string{
		"DIGITALOCEAN_ACCESS_TOKEN": "do-secret-token",
		"DIGITALOCEAN_CONTEXT":      "ambient-should-be-ignored",
		"KUBECONFIG":                "/tmp/secret-kubeconfig",
		"KUBECONTEXT":               "ambient-context",
		"DOCTL_TOKEN":               "doctl-secret",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	locs := []ref.Locator{
		{Scheme: "dir", Target: "."},
		{Scheme: "k8s", Target: "pods", ScopeKind: "k8s", ScopeValue: "ctx"},
		{Scheme: "cloud", Target: "droplets", ScopeKind: "cloud", ScopeValue: "ctx"},
		{Scheme: "dir", Target: ".", ScopeKind: "host", ScopeValue: "peer"},
		{Scheme: "sphere", Target: ""},
	}
	for _, loc := range locs {
		for _, got := range []string{TierOf(loc), TransportOf(loc)} {
			for _, secret := range secrets {
				if strings.Contains(got, secret) {
					t.Errorf("%v: %q leaks a credential", loc, got)
				}
			}
		}
	}
}
