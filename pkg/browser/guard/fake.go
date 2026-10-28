//go:build !bashy_guard_fake

package guard

// fakeBuild is false in every normal build: the fake-ask seam is honoured
// only inside a `go test` binary.
const fakeBuild = false
