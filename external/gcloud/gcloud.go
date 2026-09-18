// Package gcloud is a thin, cross-platform PASSTHROUGH wrapper for the Google
// Cloud SDK CLI (`gcloud`) — provisioning + exec, never a reimplementation.
//
// gcloud is the Python-based Google Cloud SDK, distributed by Google as a
// per-platform archive (not a single-binary GitHub release), and it self-updates
// via `gcloud components update`. So the trusted path is the VENDOR install:
// bashy prefers a gcloud already on the host (registry.Entry.PreferHost) and,
// when absent, points the operator at the official installer rather than
// TOFU-downloading a large unsigned SDK — which bashy's supply-chain policy
// forbids (no unverified binaries). All args pass through unchanged.
//
// Spin-out candidate: if a second consumer ever needs cross-platform gcloud
// provisioning in Go, this package can be lifted verbatim into a standalone
// public repo (qiangli/gcloud) with the registry entry re-pointed — the API
// boundary is already clean. Until a second consumer exists, it lives in-tree
// beside external/{kubectl,helm,ollama,podman}; a separate repo would only add
// submodule/pin overhead (see docs/email-workspace-access-strategy.md and the
// umbrella's sibling-dep discipline).
package gcloud

import (
	"context"
	"fmt"
	"runtime"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// License is the SPDX id of the Google Cloud SDK. Recorded for `bashy doctor`;
// download+exec ≠ bundle, so nothing propagates.
const License = "Apache-2.0"

// InstallHint returns the vendor-recommended install path for the current OS.
func InstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "install the Google Cloud SDK — `brew install --cask google-cloud-sdk` or https://cloud.google.com/sdk/docs/install"
	case "windows":
		return "install the Google Cloud SDK — https://cloud.google.com/sdk/docs/install (Windows installer), then reopen the shell"
	default:
		return "install the Google Cloud SDK — https://cloud.google.com/sdk/docs/install (or your distro package), then ensure `gcloud` is on PATH"
	}
}

// Resolve is the managed-provisioning entry point, consulted ONLY when no host
// gcloud is present (the registry entry sets PreferHost, so the host copy is used
// first). bashy does not auto-download the Cloud SDK: it is a large, Python-based,
// non-single-binary distribution whose vendor installer is the trusted, self-
// updating path, and TOFU-downloading it would violate the no-unverified-binary
// supply-chain rule. So this returns an actionable install hint, not a Tool.
//
// The seam for verified provisioning is ready if ever wanted: build a
// binmgr.URLSpec against
//
//	https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-sdk-<ver>-<os>-<arch>.tar.gz
//
// with Tree (whole-archive) extraction and Entrypoint "google-cloud-sdk/bin/gcloud".
// The only blocker is a trusted per-download checksum source, not the mechanism.
func Resolve(ctx context.Context, version string) (binmgr.Tool, error) {
	return binmgr.Tool{}, fmt.Errorf("gcloud not found on PATH — %s", InstallHint())
}
