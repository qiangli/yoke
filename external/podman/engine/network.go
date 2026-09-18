package engine

import (
	"context"
	"strings"

	"github.com/qiangli/yoke/pkg/oci/bindings/network"
	nettypes "github.com/qiangli/yoke/pkg/oci/nettypes"
)

// CreateNetwork creates a bridge network for the ycode session via REST API.
//
// DNSEnabled=true is required so containers attached to the network can
// resolve each other by container name (powered by aardvark-dns on the
// host). Without it, multi-container compositions can't reach each other
// — `getent hosts db` would fail even though both containers share the
// network. This matches what `podman network create <name>` does by
// default at the CLI; we set it explicitly because the libnetwork
// zero-value defaults to false.
func (e *Engine) CreateNetwork(ctx context.Context, name string) error {
	net := nettypes.Network{
		Name:       name,
		Driver:     "bridge",
		DNSEnabled: true,
	}
	_, err := network.Create(e.connCtx, &net)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return err
	}
	return nil
}

// RemoveNetwork removes a named network via REST API.
func (e *Engine) RemoveNetwork(ctx context.Context, name string) error {
	_, err := network.Remove(e.connCtx, name, nil)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no such") {
			return nil
		}
		return err
	}
	return nil
}

// ListNetworks lists networks matching the given name filter via REST API.
func (e *Engine) ListNetworks(ctx context.Context, nameFilter string) ([]NetworkInfo, error) {
	opts := new(network.ListOptions)
	if nameFilter != "" {
		opts = opts.WithFilters(map[string][]string{"name": {nameFilter}})
	}

	listed, err := network.List(e.connCtx, opts)
	if err != nil {
		return nil, err
	}

	var infos []NetworkInfo
	for _, n := range listed {
		infos = append(infos, NetworkInfo{
			Name:   n.Name,
			ID:     n.ID,
			Driver: n.Driver,
		})
	}
	return infos, nil
}

// NetworkInfo holds information about a podman network.
type NetworkInfo struct {
	Name   string `json:"Name"`
	ID     string `json:"Id"`
	Driver string `json:"Driver"`
}
