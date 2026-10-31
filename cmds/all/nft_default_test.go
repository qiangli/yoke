//go:build !bashy_dks_runtime

package all

import (
	"testing"

	"github.com/qiangli/coreutils/tool"
	_ "github.com/qiangli/yoke/external/podman/engine"
)

func TestEmbeddedPodmanDoesNotRegisterNFTHostCommand(t *testing.T) {
	if tool.Lookup("nft") != nil {
		t.Fatal("default build shadows the host nft")
	}
}
