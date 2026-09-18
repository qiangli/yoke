package engine

import (
	"testing"
)

func TestPodOptions(t *testing.T) {
	opts := &PodOptions{
		Name:    "test-pod",
		Network: "test-network",
		Labels: map[string]string{
			"ycode.session": "abc123",
		},
	}
	if opts.Name != "test-pod" {
		t.Error("unexpected name")
	}
	if opts.Network != "test-network" {
		t.Error("unexpected network")
	}
	if opts.Labels["ycode.session"] != "abc123" {
		t.Error("unexpected label")
	}
}

func TestPodInfo(t *testing.T) {
	info := PodInfo{
		ID:     "pod-123",
		Name:   "test-pod",
		Status: "Running",
	}
	if info.Status != "Running" {
		t.Error("unexpected status")
	}
}
