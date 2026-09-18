package engine

import (
	"context"
	"fmt"

	"github.com/qiangli/yoke/pkg/oci/bindings/pods"
	entTypes "github.com/qiangli/yoke/pkg/oci/entities"
	nettypes "github.com/qiangli/yoke/pkg/oci/nettypes"
	"github.com/qiangli/yoke/pkg/oci/specgen"
)

// PodOptions holds configuration for creating a pod.
type PodOptions struct {
	Name    string            // pod name
	Network string            // network name
	Labels  map[string]string // pod labels
}

// PodInfo holds information about a pod.
type PodInfo struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Status string `json:"Status"`
}

// CreatePod creates a new pod for grouping related agent containers via REST API.
func (e *Engine) CreatePod(ctx context.Context, opts *PodOptions) (string, error) {
	podSpec := specgen.PodSpecGenerator{
		PodBasicConfig: specgen.PodBasicConfig{
			Name:   opts.Name,
			Labels: opts.Labels,
		},
	}

	if opts.Network != "" {
		podSpec.Networks = map[string]nettypes.PerNetworkOptions{
			opts.Network: {},
		}
	}

	spec := &entTypes.PodSpec{
		PodSpecGen: podSpec,
	}

	resp, err := pods.CreatePodFromSpec(e.connCtx, spec)
	if err != nil {
		return "", fmt.Errorf("create pod: %w", err)
	}
	return resp.Id, nil
}

// StartPod starts all containers in a pod via REST API.
func (e *Engine) StartPod(ctx context.Context, nameOrID string) error {
	_, err := pods.Start(e.connCtx, nameOrID, nil)
	return err
}

// StopPod stops all containers in a pod via REST API.
func (e *Engine) StopPod(ctx context.Context, nameOrID string) error {
	_, err := pods.Stop(e.connCtx, nameOrID, nil)
	return err
}

// RemovePod removes a pod and all its containers via REST API.
func (e *Engine) RemovePod(ctx context.Context, nameOrID string, force bool) error {
	opts := new(pods.RemoveOptions).WithForce(force)
	_, err := pods.Remove(e.connCtx, nameOrID, opts)
	return err
}

// ListPods lists pods matching the given name filter via REST API.
func (e *Engine) ListPods(ctx context.Context, nameFilter string) ([]PodInfo, error) {
	opts := new(pods.ListOptions)
	if nameFilter != "" {
		opts = opts.WithFilters(map[string][]string{"name": {nameFilter}})
	}

	listed, err := pods.List(e.connCtx, opts)
	if err != nil {
		return nil, err
	}

	var infos []PodInfo
	for _, p := range listed {
		infos = append(infos, PodInfo{
			ID:     p.Id,
			Name:   p.Name,
			Status: p.Status,
		})
	}
	return infos, nil
}
