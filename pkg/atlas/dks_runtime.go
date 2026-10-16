// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build bashy_dks_runtime

package atlas

// DKSRuntime reports whether this build carries the DKS runtime image's k3s
// agent host tools (nft, mount, umount — cmds/all/dks_runtime.go). Ordinary
// bashy builds leave them out so the host's own commands keep resolving.
const DKSRuntime = true
