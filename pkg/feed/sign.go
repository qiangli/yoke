package feed

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

const sigVersionTag = "dhnt-feed-sig-v1"

// SigningPayload returns the canonical, deterministic byte string that a post
// signs. Layout: the version tag line "dhnt-feed-sig-v1\n", then each field as
// a 4-byte big-endian byte length followed by the raw bytes, in this order:
// bodySHA256, headline, dek, a 4-byte big-endian label count, each label in
// the given order (no sorting), and reply_to. Length prefixes keep the
// encoding unambiguous across field boundaries.
func SigningPayload(bodySHA256, headline, dek string, labels []string, replyTo string) []byte {
	buf := make([]byte, 0, len(sigVersionTag)+1+len(bodySHA256)+len(headline)+len(dek)+len(replyTo)+4+len(labels)*8)
	buf = append(buf, sigVersionTag...)
	buf = append(buf, '\n')
	appendField := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		buf = append(buf, n[:]...)
		buf = append(buf, s...)
	}
	appendField(bodySHA256)
	appendField(headline)
	appendField(dek)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(labels)))
	buf = append(buf, n[:]...)
	for _, label := range labels {
		appendField(label)
	}
	appendField(replyTo)
	return buf
}

// SigningPayloadForPost extends SigningPayload with the post's attachment
// list (each entry's path and sha256, in frontmatter order). When atts is
// empty the result is byte-identical to SigningPayload with the same fields,
// so posts without attachments sign and verify exactly as before.
func SigningPayloadForPost(bodySHA256, headline, dek string, labels []string, replyTo string, atts []Attachment) []byte {
	buf := SigningPayload(bodySHA256, headline, dek, labels, replyTo)
	if len(atts) == 0 {
		return buf
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(atts)))
	buf = append(buf, n[:]...)
	for _, a := range atts {
		var m [4]byte
		binary.BigEndian.PutUint32(m[:], uint32(len(a.Path)))
		buf = append(buf, m[:]...)
		buf = append(buf, a.Path...)
		binary.BigEndian.PutUint32(m[:], uint32(len(a.SHA256)))
		buf = append(buf, m[:]...)
		buf = append(buf, a.SHA256...)
	}
	return buf
}

// SignPayload signs payload with priv and returns the base64 (std encoding)
// public key and signature.
func SignPayload(priv ed25519.PrivateKey, payload []byte) (pubB64, sigB64 string) {
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, payload)
	return base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(sig)
}

// VerifyPayload checks a base64-encoded ed25519 public key and signature over
// payload. It errors on invalid base64, wrong key or signature length, or a
// signature mismatch.
func VerifyPayload(pubB64, sigB64 string, payload []byte) error {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return fmt.Errorf("feed: public key is not valid base64: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("feed: public key has wrong length: want %d bytes, got %d", ed25519.PublicKeySize, len(pub))
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("feed: signature is not valid base64: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("feed: signature has wrong length: want %d bytes, got %d", ed25519.SignatureSize, len(sig))
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), payload, sig) {
		return errors.New("feed: signature verification failed")
	}
	return nil
}
