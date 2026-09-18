package engine

import (
	"testing"
)

func TestHostGateway(t *testing.T) {
	// Engine's HostGateway should return the podman host gateway.
	e := &Engine{}
	gw := e.HostGateway()
	if gw != "host.containers.internal" {
		t.Errorf("expected host.containers.internal, got %q", gw)
	}
}

func TestNetworkInfo(t *testing.T) {
	info := NetworkInfo{
		Name:   "test-network",
		ID:     "abc123",
		Driver: "bridge",
	}
	if info.Name != "test-network" {
		t.Error("unexpected name")
	}
	if info.Driver != "bridge" {
		t.Error("unexpected driver")
	}
}
