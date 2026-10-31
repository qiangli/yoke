// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

//go:build bashy_dks_runtime

package all

// The k3s agent host tools of the FROM-scratch DKS runtime image, whose bashy
// is built with -tags bashy_dks_runtime. General command registration stays
// out of every other build to avoid shadowing host nft/mount/umount.
import (
	_ "github.com/qiangli/yoke/cmds/mount"
	nftcmd "github.com/qiangli/yoke/cmds/nft"
)

func init() { nftcmd.Register() }
