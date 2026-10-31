//go:build linux

package nftcmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/qiangli/yoke/pkg/nft"
)

func TestNetavarkJSONNetlink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root and CAP_NET_ADMIN")
	}
	b, err := nft.NewNetlink()
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("CAP_NET_ADMIN unavailable")
		}
		t.Fatal(err)
	}
	table := fmt.Sprintf("bashy_netavark_test_%d", os.Getpid())
	tx := fmt.Sprintf(`{"nftables":[{"add":{"table":{"family":"inet","name":%q}}},{"add":{"chain":{"family":"inet","table":%q,"name":"POSTROUTING","type":"nat","hook":"postrouting","prio":100,"policy":"accept"}}},{"add":{"rule":{"family":"inet","table":%q,"chain":"POSTROUTING","expr":[{"match":{"left":{"payload":{"protocol":"ip","field":"saddr"}},"right":{"prefix":{"addr":"10.88.0.0","len":16}},"op":"=="}},{"match":{"left":{"payload":{"protocol":"ip","field":"daddr"}},"right":{"prefix":{"addr":"224.0.0.0","len":4}},"op":"!="}},{"masquerade":null}]}}}]}`, table, table, table)
	if err := nft.RunJSON(b, []byte(tx), false); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("CAP_NET_ADMIN unavailable: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := nft.NewNetlink()
		if err == nil {
			_ = nft.Run(cleanup, "delete table inet "+table+"\n", false)
		}
	})
	read, err := nft.NewNetlink()
	if err != nil {
		t.Fatal(err)
	}
	out, err := nft.ListJSON(read, nft.ListArgs{What: "table", Family: nft.FamilyInet, Table: table})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"masquerade":null`) || !strings.Contains(string(out), `"chain":"POSTROUTING"`) {
		t.Fatalf("netlink ruleset missing netavark rule: %s", out)
	}
}

func TestCapturedNetavarkJSONNetlink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root and CAP_NET_ADMIN")
	}
	b, err := nft.NewNetlink()
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("CAP_NET_ADMIN unavailable")
		}
		t.Fatal(err)
	}
	table := fmt.Sprintf("bashy_nft_capture_%d", os.Getpid())
	t.Cleanup(func() {
		cleanup, err := nft.NewNetlink()
		if err == nil {
			_ = nft.Run(cleanup, "delete table inet "+table+"\n", false)
		}
	})
	for _, name := range []string{"netavark-bridge.json", "netavark-portmap.json"} {
		input, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		// Give the captured transaction a private table so it cannot touch
		// the host's live Podman ruleset.
		input = []byte(strings.ReplaceAll(string(input), `"netavark"`, fmt.Sprintf("%q", table)))
		if err := nft.RunJSON(b, input, false); err != nil {
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				t.Skipf("CAP_NET_ADMIN unavailable: %v", err)
			}
			t.Fatalf("%s: %v", name, err)
		}
	}
	read, err := nft.NewNetlink()
	if err != nil {
		t.Fatal(err)
	}
	out, err := nft.ListJSON(read, nft.ListArgs{What: "table", Family: nft.FamilyInet, Table: table})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"masquerade":null`) || !strings.Contains(string(out), `"dnat":{`) {
		t.Fatalf("captured rules missing from kernel: %s", out)
	}
}
