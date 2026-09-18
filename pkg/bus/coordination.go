package bus

import (
	"fmt"
	"unicode/utf8"
)

// MaxCoordinationBodyBytes is the admission limit for one user/agent-authored
// quick coordination message. It is a byte limit, not a rune limit, because
// durable stores and transport envelopes carry UTF-8 bytes.
const MaxCoordinationBodyBytes = 1024

// ValidateCoordinationBody rejects an oversized quick-coordination body. It
// never truncates or splits: either the caller can append the exact body, or it
// appends nothing.
func ValidateCoordinationBody(body string) error {
	if !utf8.ValidString(body) {
		return fmt.Errorf("coordination body is not valid UTF-8; use a valid UTF-8 request/priority/owner plus a stable reachable repo-relative path+commit, issue, room, or artifact reference")
	}
	n := len(body)
	if n <= MaxCoordinationBodyBytes {
		return nil
	}
	return fmt.Errorf("coordination body is %d UTF-8 bytes; max %d; shorten to request/priority/owner plus a stable reachable repo-relative path+commit, issue, room, or artifact reference",
		n, MaxCoordinationBodyBytes)
}
