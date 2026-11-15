package ask

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// SourceGateway marks approvals raised by the vendor-chat gateway effect
// gate (docs/vendor-chat-gateway-mvp.md, decision 3). A `run` call or worker
// action whose effects include destroy, spend, cred or priv raises one such
// approval instead of executing, on the sync and pull routes alike.
const SourceGateway = "gateway"

// GatewayDigest binds a gateway approval to its candidate: the exact argv,
// the owning host and the gateway task id. The approve directive carries
// this digest back as the plan token, and ApplyRemote settles the decision
// only when the carried token still equals the raised one — a stale digest
// (the candidate changed, or the answer is for another host or task) fails
// closed. Each element is length-prefixed before hashing, so element
// boundaries always change the digest and no payload byte can alias one.
func GatewayDigest(argv []string, host, taskID string) string {
	parts := make([]string, 0, len(argv)+2)
	parts = append(parts, host, taskID)
	parts = append(parts, argv...)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
