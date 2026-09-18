// Package stack manages ports for the embedded OTEL stack.
package stack

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// PortAllocator allocates random free ports on 127.0.0.1 and persists
// the assignments to a JSON file for cross-process discoverability.
type PortAllocator struct {
	mu    sync.Mutex
	ports map[string]int
	path  string // path to ports.json
}

// NewPortAllocator creates a port allocator persisting to dir/ports.json.
func NewPortAllocator(dir string) *PortAllocator {
	pa := &PortAllocator{
		ports: make(map[string]int),
		path:  filepath.Join(dir, "ports.json"),
	}
	// Load existing allocations (best-effort).
	data, err := os.ReadFile(pa.path)
	if err == nil {
		_ = json.Unmarshal(data, &pa.ports)
	}
	return pa
}

// Allocate finds a free port on 127.0.0.1 for the named component,
// records it, and returns the port number.
func (pa *PortAllocator) Allocate(component string) (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate port for %s: %w", component, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	pa.ports[component] = port
	pa.persist()
	return port, nil
}

// Get returns the allocated port for a component, or 0 if not allocated.
func (pa *PortAllocator) Get(component string) int {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	return pa.ports[component]
}

// All returns a copy of all port allocations.
func (pa *PortAllocator) All() map[string]int {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	result := make(map[string]int, len(pa.ports))
	for k, v := range pa.ports {
		result[k] = v
	}
	return result
}

// Release removes a port allocation.
func (pa *PortAllocator) Release(component string) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	delete(pa.ports, component)
	pa.persist()
}

// ReleaseAll removes all port allocations and deletes the ports file.
func (pa *PortAllocator) ReleaseAll() {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	pa.ports = make(map[string]int)
	os.Remove(pa.path)
}

// AllocatePort finds a free TCP port on 127.0.0.1 and returns it.
// The port is released immediately so the caller can pass it to a component.
func AllocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// IsPortAvailable returns true if the given TCP port on 127.0.0.1 is free.
func IsPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func (pa *PortAllocator) persist() {
	data, err := json.MarshalIndent(pa.ports, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(pa.path), 0o755)
	_ = os.WriteFile(pa.path, data, 0o644)
}
