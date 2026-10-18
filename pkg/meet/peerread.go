package meet

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/lens"
)

// peerOSFor reports a peer host's OS ("windows", "linux", ...) or "". The
// default knows nothing, so peerReadLine renders both dialects labeled; a host
// wired to outpost's peer status can set it. It must not touch the network on its own.
var peerOSFor = func(host string) string { return "" }

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
