// Package procfs is a clean-room replacement for the procfs API of
// github.com/cyphar/filepath-securejoin/pathrs-lite, implementing only
// what the shipped Podman engine graph calls (opencontainers/selinux,
// runc internal/pathrs). Original yoke code (MIT); see
// ../../README.yoke.md.
package procfs

// ProcThreadSelfCloser must be called once the caller is done with a
// handle from [Handle.OpenThreadSelf]: it releases the OS thread the
// goroutine was locked to so the thread-self path stayed meaningful.
type ProcThreadSelfCloser func()
