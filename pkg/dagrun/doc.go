// Package dagrun turns a registered command plus the argv and context it was
// typed in into ONE bashy run manifest, and renders that manifest for the
// cluster tier. Design of record: the "DKS cluster-userland execution
// contract" (Draft §2, Decision of record §4 and §8).
//
// The manifest is an ordinary yoke/pkg/dag document: frontmatter (`type:
// dag`, run facts under `metadata:`) and one target, `run`, whose metadata
// lines are existing dag keys and whose sh body is the argv re-quoted by the
// shell's own quoting. The same bytes run unchanged on every tier; only the
// cluster executor renders them, at the edge.
//
// Generate and Render are pure: no I/O, no clock, no randomness (the caller
// supplies run ids), sorted keys, byte-identical output for the same input.
// Both fail closed. Render returns plain unstructured objects; this package
// links no Kubernetes client. Linux is the only rendered executor class for
// now; native macOS/Windows return ErrUnsupportedExecutor.
package dagrun
