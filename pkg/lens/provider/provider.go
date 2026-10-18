// Package provider defines the read-only seam for addressable trees and graphs.
package provider

import (
	"context"
	"errors"
	"io"

	"github.com/qiangli/yoke/pkg/ref"
)

const NodeSchema = "bashy-node-v1"
const EdgeSchema = "bashy-edge-v1"

type Node struct {
	SchemaVersion string         `json:"schema_version"`
	Ref           string         `json:"ref"`
	Path          string         `json:"path"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	Type          string         `json:"type"`
	Size          *int64         `json:"size,omitempty"`
	Mode          string         `json:"mode,omitempty"`
	Mtime         string         `json:"mtime,omitempty"`
	Rev           string         `json:"rev,omitempty"`
	Children      *uint64        `json:"children,omitempty"`
	Attrs         map[string]any `json:"attrs"`
}

type Edge struct {
	SchemaVersion string         `json:"schema_version"`
	Src           string         `json:"src"`
	Rel           string         `json:"rel"`
	Dst           string         `json:"dst"`
	Attrs         map[string]any `json:"attrs"`
}

var ErrUnsupported = errors.New("provider: unsupported operation")

type Provider interface {
	List(context.Context, ref.Locator) ([]Node, error)
	Stat(context.Context, ref.Locator) (Node, error)
	Open(context.Context, ref.Locator) (io.ReadCloser, error)
	Edges(context.Context, ref.Locator) ([]Edge, error)
}

type Registry struct{ providers map[string]Provider }

func NewRegistry() *Registry { return &Registry{providers: map[string]Provider{}} }
func (r *Registry) Register(scheme string, p Provider) error {
	if scheme == "" || p == nil {
		return errors.New("provider: scheme and provider required")
	}
	if _, exists := r.providers[scheme]; exists {
		return errors.New("provider: duplicate scheme " + scheme)
	}
	r.providers[scheme] = p
	return nil
}
func (r *Registry) Lookup(scheme string) (Provider, bool) { p, ok := r.providers[scheme]; return p, ok }

// Parse applies only schemes registered here in addition to built-in grammar.
func (r *Registry) Parse(s string) (ref.Locator, error) {
	extra := make(map[string]bool, len(r.providers))
	for k := range r.providers {
		extra[k] = true
	}
	return ref.ParseLocatorWithSchemes(s, extra)
}
