package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/lens"
)

// peerOSFor resolves the OS reported by the existing outpost peer board. An
// unavailable or ambiguous board leaves the dialect unknown, so the read line
// offers both forms instead of guessing.
var peerOSFor = lookupPeerOS

var peerStatusRunner = runPeerStatus

var errPeerStatusUnavailable = errors.New("outpost peer status unavailable")

var peerStatusCache struct {
	sync.Mutex
	data  []byte
	until time.Time
}

type peerOSRow struct {
	Host  string `json:"host"`
	Alias string `json:"alias"`
	OS    string `json:"os"`
}

type peerOutput struct {
	bytes.Buffer
	overflow bool
}

func (o *peerOutput) Write(p []byte) (int, error) {
	n := len(p)
	if room := (1 << 20) - o.Len(); len(p) > room {
		o.overflow = true
		p = p[:room]
	}
	_, _ = o.Buffer.Write(p)
	return n, nil
}

// runPeerStatus only reads peer metadata. The deadline and output cap keep
// rendering a Meet transcript bounded when outpost is unavailable.
func runPeerStatus(ctx context.Context) ([]byte, error) {
	peerStatusCache.Lock()
	defer peerStatusCache.Unlock()
	if time.Now().Before(peerStatusCache.until) {
		if peerStatusCache.data == nil {
			return nil, errPeerStatusUnavailable
		}
		return peerStatusCache.data, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var out peerOutput
	cmd := exec.CommandContext(ctx, "outpost", "peers", "status", "--json")
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	peerStatusCache.until = time.Now().Add(10 * time.Second)
	peerStatusCache.data = nil
	if err := cmd.Run(); err != nil || out.overflow {
		return nil, errPeerStatusUnavailable
	}
	peerStatusCache.data = out.Bytes()
	return peerStatusCache.data, nil
}

func lookupPeerOS(host string) string {
	if host == "" {
		return ""
	}
	raw, err := peerStatusRunner(context.Background())
	if err != nil {
		return ""
	}
	var rows []peerOSRow
	if json.Unmarshal(raw, &rows) != nil {
		return ""
	}
	var found string
	matched := false
	for _, row := range rows {
		if !strings.EqualFold(host, row.Host) && (row.Alias == "" || !strings.EqualFold(host, row.Alias)) {
			continue
		}
		if matched { // duplicate host or alias: do not guess
			return ""
		}
		matched = true
		found = strings.ToLower(row.OS)
	}
	switch found {
	case "windows", "linux", "darwin":
		return found
	}
	return ""
}

// peerReadLine renders the read-only follow command for a cross-host locator
// in the peer's command-line dialect (the S4 argv rows: bash, or pwsh for a
// Windows peer), or "" for a same-host locator. The locator already carries
// its @host scope, so the command reads through the peer transport.
func peerReadLine(r bus.MessageRef) string {
	if !r.CrossHost {
		return ""
	}
	switch strings.ToLower(peerOSFor(r.Host)) {
	case "windows":
		return peerDialectLine(r, "pwsh")
	case "":
		// Unknown peer OS: never claim POSIX; offer both, labeled.
		b, p := peerDialectLine(r, "bash"), peerDialectLine(r, "pwsh")
		if b == "" || p == "" {
			return b + p
		}
		return "[bash] " + b + " | [pwsh] " + p
	}
	return peerDialectLine(r, "bash")
}

func peerDialectLine(r bus.MessageRef, to string) string {
	data, err := json.Marshal(map[string]any{"name": "lens", "kind": "command", "exec": []string{"lens", r.Ref}})
	if err != nil {
		return ""
	}
	res, err := lens.Convert(context.Background(), lens.Request{From: "json", To: to, Data: data})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(res.Data))
}
