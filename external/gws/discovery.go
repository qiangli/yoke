// Runtime Discovery schema inspection and cache drift for gws
// (Sprint 457 story f25d7aa4437c).
//
// Google's Workspace APIs are described by runtime Discovery documents; a
// binary pin does NOT freeze the remote API, so every listing records which
// Discovery revision it was observed under (see Binding.Revision in page.go)
// and re-inspection fails closed on drift instead of silently mixing
// revisions. This file is the offline inspection half: it parses a
// Discovery document already in hand (fixture, cache, or a just-fetched
// body the caller supplies) and compares fingerprints. Fetching a live
// document needs an authorized test account and is owned by Sprint 465
// story 534ede495eb8 — nothing here touches the network.
package gws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Discovery is the inspected identity of one Google API Discovery document:
// which service, which version, which revision, and the fingerprint of the
// exact bytes observed. Record it alongside cached listings and page
// bindings so a later observation can prove it is still the same schema.
type Discovery struct {
	Name        string
	Version     string
	Revision    string
	Fingerprint string
}

// InspectDiscovery parses raw Discovery document bytes and returns their
// identity. It fails with a typed invalid-input error on malformed or
// identity-less documents — never a partial identity. The fingerprint is
// the sha256 of the exact bytes observed, so any upstream schema change,
// however small, changes it.
func InspectDiscovery(raw []byte) (Discovery, *Error) {
	if len(raw) == 0 {
		return Discovery{}, NewError(CodeInvalidInput, "gws: empty Discovery document — re-inspect explicitly", nil)
	}
	var doc struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Discovery{}, NewError(CodeInvalidInput, fmt.Sprintf("gws: malformed Discovery document: %v — re-inspect explicitly", err), nil)
	}
	if doc.Name == "" || doc.Version == "" {
		return Discovery{}, NewError(CodeInvalidInput, "gws: Discovery document names no service (want name and version) — re-inspect explicitly", nil)
	}
	sum := sha256.Sum256(raw)
	return Discovery{
		Name:        doc.Name,
		Version:     doc.Version,
		Revision:    doc.Revision,
		Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

// CheckDrift compares a cached Discovery identity against a freshly
// observed one for the same service. Nil means the cache still describes
// the live schema; a typed error means the schema drifted (or the observed
// document is for a different service/version) and the caller must
// re-inspect and proceed under the new revision explicitly — never silently
// mix revisions. Drift is not retryable: retrying under the stale revision
// cannot succeed.
func CheckDrift(cached, observed Discovery) *Error {
	if observed.Name != cached.Name || observed.Version != cached.Version {
		return NewError(CodeProviderFailure, fmt.Sprintf("gws: Discovery identity changed (was %s %s, observed %s %s) — re-inspect and proceed under the new revision explicitly", cached.Name, cached.Version, observed.Name, observed.Version), nil)
	}
	if observed.Fingerprint != cached.Fingerprint {
		err := NewError(CodeProviderFailure, fmt.Sprintf("gws: Discovery schema drift for %s %s (revision %q -> %q) — re-inspect and proceed under the new revision explicitly", cached.Name, cached.Version, cached.Revision, observed.Revision), nil)
		err.Retryable = false
		return err
	}
	return nil
}
