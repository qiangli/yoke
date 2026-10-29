// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package shape is the parent of the shaping trust roots. The per-app,
// per-principal state record lives in its state child package; the
// guards candidate units run under (and the route to bashy oci for a unit
// that asks for isolation) live in contain; the app data boundary the
// unit's interpreter enforces, and the cross-app call path through the
// owning app's interface, live in boundary.
package shape
