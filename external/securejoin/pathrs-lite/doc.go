// Package pathrs is a clean-room replacement for
// github.com/cyphar/filepath-securejoin/pathrs-lite, implementing only
// the API the shipped Podman engine graph calls: podman libpod and
// play, containers/storage, opencontainers/selinux and runc
// internal/pathrs. Original yoke code (MIT); see ../README.yoke.md.
//
// Every lookup is file-descriptor based. On Linux 5.6+ the kernel
// resolves the whole path with openat2(2) RESOLVE_IN_ROOT, so ".." and
// symlinks (absolute or relative) are scoped to the root atomically;
// older kernels get an openat(O_NOFOLLOW) walk that holds a handle for
// every directory it passes through and resolves ".." by dropping back
// to the held parent, never by a lexical join on a pathname. Returned
// handles are O_PATH; use [Reopen] to get a readable/writable file.
package pathrs
