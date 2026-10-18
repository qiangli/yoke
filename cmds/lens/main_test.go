package lenscmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

// Fence the entire package: room emissions can also notify the message board.
func TestMain(m *testing.M) {
	testRoot, err := os.MkdirTemp("", "yoke-lens-tests-*")
	if err != nil {
		panic(err)
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BASHY_") && (strings.HasSuffix(key, "_DIR") || key == "BASHY_HOME") {
			os.Unsetenv(key)
		}
	}
	for key, value := range map[string]string{
		"HOME": testRoot, "USERPROFILE": testRoot,
		"BASHY_HOME":     filepath.Join(testRoot, "bashy"),
		"BASHY_MB_DIR":   filepath.Join(testRoot, "mb"),
		"BASHY_ROOM_DIR": filepath.Join(testRoot, "room"),
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	var dials atomic.Int64
	bus.RemoteSend = func(bus.RemoteMessage) (bus.RemoteReceipt, error) {
		dials.Add(1)
		return bus.RemoteReceipt{}, fmt.Errorf("lens test attempted an unstubbed relay send")
	}
	code := m.Run()
	if dials.Load() != 0 {
		fmt.Fprintln(os.Stderr, "FAIL: lens tests attempted relay sends")
		code = 1
	}
	os.RemoveAll(testRoot)
	os.Exit(code)
}
