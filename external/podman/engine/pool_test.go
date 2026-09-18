package engine

import (
	"testing"
)

func TestPoolCreation(t *testing.T) {
	template := &ContainerConfig{
		Name:  "test-pool",
		Image: "test:latest",
	}
	pool := NewPool(nil, template, 5)

	if pool.Size() != 5 {
		t.Errorf("expected size 5, got %d", pool.Size())
	}
	if pool.Available() != 0 {
		t.Errorf("expected 0 available, got %d", pool.Available())
	}
}
