// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package atlas_test

import (
	"slices"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

var dksRuntimeNames = []string{"nft", "mount", "umount"}

// dksRuntimeTools is how many tool rows the bashy_dks_runtime tag adds; the
// count pins elsewhere in this package add it.
func dksRuntimeTools() int {
	if atlas.DKSRuntime {
		return len(dksRuntimeNames)
	}
	return 0
}

// The DKS runtime image's host tools must NOT resolve in an ordinary bashy
// build (they would shadow the host's real nft/mount/umount); with
// -tags bashy_dks_runtime, cmds/all registers them and atlas lists them.
func TestDKSRuntimeToolsGatedByTag(t *testing.T) {
	registered := tool.Names()
	for _, n := range dksRuntimeNames {
		_, inAtlas := atlas.Lookup(n)
		if got := slices.Contains(registered, n); got != atlas.DKSRuntime || inAtlas != atlas.DKSRuntime {
			t.Errorf("%s: registered=%v atlas=%v, want both %v (bashy_dks_runtime tag)", n, got, inAtlas, atlas.DKSRuntime)
		}
	}
}
