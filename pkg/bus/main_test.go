// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package bus

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

var relayDials []string

// TestMain fences every pkg/bus test into a private home and installs a
// default relay that records (and fails the run on) any dial a test did not
// stub itself, so no test can post to the operator's real mailbox or relay.
func TestMain(m *testing.M) {
	home := ""
	// Re-exec'd helper children inherit the fenced env their parent test built.
	if os.Getenv("YOKE_BUS_TEST_FENCED") != "1" {
		var err error
		if home, err = os.MkdirTemp("", "yoke-bus-test-*"); err != nil {
			panic(err)
		}
		os.Setenv("HOME", home)
		os.Setenv("USERPROFILE", home)
		for _, e := range os.Environ() {
			k, _, _ := strings.Cut(e, "=")
			if strings.HasPrefix(k, "BASHY_") && (strings.HasSuffix(k, "_DIR") || k == "BASHY_HOME") {
				os.Unsetenv(k)
			}
		}
		os.Setenv("YOKE_BUS_TEST_FENCED", "1")
	}
	guard := func(m RemoteMessage) (RemoteReceipt, error) {
		relayDials = append(relayDials, m.To)
		return RemoteReceipt{}, fmt.Errorf("test dialed the real relay for %s", m.To)
	}
	RemoteSend = guard
	code := m.Run()
	if home != "" {
		os.RemoveAll(home)
	}
	if len(relayDials) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: unstubbed relay dials: %v\n", relayDials)
		code = 1
	}
	os.Exit(code)
}
