//go:build linux || freebsd

// Package server re-exports Podman REST API server.
package server

import "go.podman.io/podman/v6/pkg/api/server"

var NewServerWithSettings = server.NewServerWithSettings
