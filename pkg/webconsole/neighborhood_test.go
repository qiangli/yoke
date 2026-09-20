// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sprint 220, story ed857a89: the Neighborhood is the union of the mesh's
// LAN peers and the mDNS browse, merged by host name; each source may fail
// alone; a WAN/relayed mesh peer is not a neighbour.
func TestNeighborhoodMergesMeshAndMDNS(t *testing.T) {
	prev := outpostJSON
	t.Cleanup(func() { outpostJSON = prev })
	fake := filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BIN", fake) // meshagent.Resolve → installed
	outpostJSON = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "mesh status --json":
			return []byte(`{"status":{"peers":[
			  {"id":"12D3KooWAAA","name":"winbox","direct":true,"link_class":"lan","remote":["/ip6/2601::1/udp/1/quic-v1","/ip4/10.0.0.45/udp/2/quic-v1"]},
			  {"id":"12D3KooWBBB","name":"faraway","direct":true,"link_class":"wan","remote":["/ip4/8.8.8.8/udp/3/quic-v1"]},
			  {"id":"12D3KooWCCC","direct":true,"link_class":"lan","remote":["/ip4/10.0.0.9/udp/4/quic-v1"]}]}}`), nil
		case "scan --json --timeout 2s":
			return []byte(`[{"agent_name":"winbox","os_username":"noviadmin","paired":true,"endpoints":[{"kind":"http","host":"10.0.0.45","port":17778}]},
			  {"agent_name":"guest-laptop","os_username":"guest","paired":false,"endpoints":[{"kind":"http","host":"guest-laptop.local","port":17778}]}]`), nil
		}
		return nil, errors.New("unexpected " + strings.Join(args, " "))
	}
	v := neighborhood(context.Background())
	if v.Agent != "installed" || v.Sources.Mesh != "ok" || v.Sources.MDNS != "ok" {
		t.Fatalf("sources = %+v agent=%s", v.Sources, v.Agent)
	}
	names := []string{}
	for _, h := range v.Hosts {
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "12D3KooWCCC,guest-laptop,winbox" {
		t.Fatalf("hosts = %v", names)
	}
	for _, h := range v.Hosts {
		switch h.Name {
		case "winbox":
			if strings.Join(h.Via, "+") != "mesh+mdns" || h.Address != "10.0.0.45" || h.Owner != "this-account" || h.User != "noviadmin" {
				t.Fatalf("winbox merged wrong: %+v", h)
			}
		case "guest-laptop":
			if h.Owner != "discovered" || h.Paired || h.Address != "guest-laptop.local" {
				t.Fatalf("discovered host wrong: %+v", h)
			}
		}
	}
	// mDNS alone when there is no daemon (unpaired host): still an answer.
	outpostJSON = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "mesh" {
			return nil, errors.New("admin: connection refused")
		}
		return []byte(`[{"agent_name":"guest-laptop","paired":false,"endpoints":[]}]`), nil
	}
	v = neighborhood(context.Background())
	if v.Sources.Mesh == "ok" || v.Sources.MDNS != "ok" || len(v.Hosts) != 1 {
		t.Fatalf("mdns-only = %+v", v)
	}
}

func TestNeighborhoodWithoutTheAgentSaysSo(t *testing.T) {
	t.Setenv("OUTPOST_BIN", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/api/neighborhood", "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"agent":"missing"`) {
		t.Fatalf("GET /api/neighborhood = %d %s", w.Code, w.Body.String())
	}
}

func TestLauncherShipsTheNeighborhoodSection(t *testing.T) {
	h := newTestHandler(t, Options{})
	js := do(h, "GET", "/app.js", "127.0.0.1:5555", nil).Body.String()
	for _, want := range []string{`"showNeighborhood", "Neighborhood"`, `neighborhood-section`, `api/neighborhood`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
