// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build bashy_dks_runtime

package all

// The k3s agent host tools of the FROM-scratch DKS runtime image, whose bashy
// is built with -tags bashy_dks_runtime. Kept out of every other build: there
// they would shadow the host's real nft/mount/umount.
import (
	_ "github.com/qiangli/yoke/cmds/mount"
	_ "github.com/qiangli/yoke/cmds/nft"
)
