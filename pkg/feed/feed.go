package feed

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Verification kinds. VerifyPost returns nil for a verified post, or an error
// matching exactly one of these with errors.Is.
var (
	// ErrUnsigned: the post carries no pubkey/sig at all.
	ErrUnsigned = errors.New("feed: post is unsigned")
	// ErrDrifted: the post claims a signature that does not verify over its
	// current content (body hash or excerpt fields changed, or the signature
	// is malformed).
	ErrDrifted = errors.New("feed: post has drifted from its signature")
)

func postPayload(p *Post) []byte {
	return SigningPayloadForPost(p.BodySHA256(), p.Headline, p.Dek, p.Labels, p.ReplyTo, p.Attachments)
}

// SignPost stamps p.Pubkey and p.Sig over the post's body hash and excerpt
// fields.
func SignPost(priv ed25519.PrivateKey, p *Post) error {
	if p == nil {
		return errors.New("feed: nil post")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("feed: invalid signing key")
	}
	p.Pubkey, p.Sig = SignPayload(priv, postPayload(p))
	return nil
}

// VerifyPost checks p's signature against its recomputed body hash and
// excerpt fields.
func VerifyPost(p *Post) error {
	if p == nil {
		return errors.New("feed: nil post")
	}
	if p.Pubkey == "" && p.Sig == "" {
		return ErrUnsigned
	}
	if p.Pubkey == "" || p.Sig == "" {
		return fmt.Errorf("%w: pubkey or sig missing", ErrDrifted)
	}
	if err := VerifyPayload(p.Pubkey, p.Sig, postPayload(p)); err != nil {
		return fmt.Errorf("%w: %v", ErrDrifted, err)
	}
	return nil
}

// Status strings recorded in the spool and printed by fetch.
const (
	StatusVerified = "verified"
	StatusDrifted  = "drifted"
	StatusUnsigned = "unsigned"
)

func verifyStatus(p *Post) (status, detail string) {
	err := VerifyPost(p)
	switch {
	case err == nil:
		return StatusVerified, ""
	case errors.Is(err, ErrUnsigned):
		return StatusUnsigned, ""
	default:
		return StatusDrifted, strings.TrimPrefix(err.Error(), ErrDrifted.Error()+": ")
	}
}

// subscriberName resolves the acting principal: explicit name, then
// BASHY_PRINCIPAL, USER, LOGNAME.
func subscriberName(as string) string {
	if s := strings.TrimSpace(as); s != "" {
		return s
	}
	for _, k := range []string{"BASHY_PRINCIPAL", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "anonymous"
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func safeFileName(s string) string {
	name := strings.TrimLeft(unsafeName.ReplaceAllString(s, "_"), ".")
	if name == "" {
		return "anonymous"
	}
	return name
}
