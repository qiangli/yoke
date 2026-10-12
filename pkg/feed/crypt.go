package feed

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const encKeyFileName = "x25519.key"
const wrapTag = "dhnt-feed-wrap-v1"
const fieldTag = "dhnt-feed-enc-v1"

// LoadOrCreateEncKey stores a raw X25519 private key with the signing key's permissions.
func LoadOrCreateEncKey(dir string) (*ecdh.PrivateKey, error) {
	path := filepath.Join(dir, encKeyFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := checkKeyPerms(path); err != nil {
			return nil, err
		}
		if len(raw) != 32 {
			return nil, fmt.Errorf("feed: %s: malformed encryption key: want 32 bytes, got %d", path, len(raw))
		}
		return ecdh.X25519().NewPrivateKey(raw)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("feed: read %s: %w", path, err)
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writeRawEncKey(dir, path, key.Bytes()); err != nil {
		return nil, err
	}
	return key, nil
}

func writeRawEncKey(dir, path string, raw []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".x25519.key.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func EncPubkeyB64(key *ecdh.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
}

func decodeEncPubkey(s string) (*ecdh.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("feed: encryption pubkey must be 32 bytes")
	}
	return ecdh.X25519().NewPublicKey(b)
}

func randomReader(r io.Reader) io.Reader {
	if r == nil {
		return rand.Reader
	}
	return r
}
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("feed: AES key must be 32 bytes")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
func wrapInfo(group string, epoch int) ([]byte, error) {
	if group == "" || epoch < 1 {
		return nil, errors.New("feed: group and positive epoch required")
	}
	return []byte(wrapTag + "|" + group + "|" + strconv.Itoa(epoch)), nil
}
func deriveKEK(shared, ephemeral, recipient []byte, group string, epoch int) ([]byte, error) {
	info, err := wrapInfo(group, epoch)
	if err != nil {
		return nil, err
	}
	salt := append(append(make([]byte, 0, 64), ephemeral...), recipient...)
	out := make([]byte, 32)
	_, err = io.ReadFull(hkdf.New(sha256.New, shared, salt, info), out)
	return out, err
}

// Wrap returns base64(ephemeral public key || nonce || AES-GCM ciphertext).
// A supplied reader controls the ephemeral key and nonce for deterministic tests.
func Wrap(groupKey []byte, memberPubB64, group string, epoch int, random io.Reader) (string, error) {
	if len(groupKey) != 32 {
		return "", errors.New("feed: group key must be 32 bytes")
	}
	member, err := decodeEncPubkey(memberPubB64)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(randomReader(random), raw); err != nil {
		return "", err
	}
	ephemeral, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", err
	}
	shared, err := ephemeral.ECDH(member)
	if err != nil {
		return "", err
	}
	kek, err := deriveKEK(shared, ephemeral.PublicKey().Bytes(), member.Bytes(), group, epoch)
	if err != nil {
		return "", err
	}
	aead, err := newGCM(kek)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(randomReader(random), nonce); err != nil {
		return "", err
	}
	raw = append(ephemeral.PublicKey().Bytes(), nonce...)
	raw = append(raw, aead.Seal(nil, nonce, groupKey, []byte(wrapTag))...)
	return base64.StdEncoding.EncodeToString(raw), nil
}

func Unwrap(wrapped string, member *ecdh.PrivateKey, group string, epoch int) ([]byte, error) {
	if member == nil {
		return nil, errors.New("feed: missing encryption key")
	}
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, err
	}
	if len(raw) != 92 {
		return nil, fmt.Errorf("feed: wrapped key must be 92 bytes")
	}
	ephemeral, err := ecdh.X25519().NewPublicKey(raw[:32])
	if err != nil {
		return nil, err
	}
	shared, err := member.ECDH(ephemeral)
	if err != nil {
		return nil, err
	}
	kek, err := deriveKEK(shared, raw[:32], member.PublicKey().Bytes(), group, epoch)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(kek)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, raw[32:44], raw[44:], []byte(wrapTag))
}

func fieldAAD(group string, epoch int, field string) ([]byte, error) {
	if _, err := wrapInfo(group, epoch); err != nil {
		return nil, err
	}
	switch field {
	case "body", "headline", "dek":
	default:
		return nil, fmt.Errorf("feed: invalid encrypted field %q", field)
	}
	return []byte(fieldTag + "|" + group + "|" + strconv.Itoa(epoch) + "|" + field), nil
}

func EncryptField(value string, key []byte, group string, epoch int, field string, random io.Reader) (string, error) {
	if value == "" {
		return "", nil
	}
	aad, err := fieldAAD(group, epoch, field)
	if err != nil {
		return "", err
	}
	aead, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(randomReader(random), nonce); err != nil {
		return "", err
	}
	raw := append(nonce, aead.Seal(nil, nonce, []byte(value), aad)...)
	return "enc:v1:" + base64.StdEncoding.EncodeToString(raw), nil
}

func DecryptField(value string, key []byte, group string, epoch int, field string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, "enc:v1:") {
		return "", errors.New("feed: expected enc:v1 field")
	}
	aad, err := fieldAAD(group, epoch, field)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "enc:v1:"))
	if err != nil {
		return "", err
	}
	if len(raw) < 28 {
		return "", errors.New("feed: encrypted field too short")
	}
	aead, err := newGCM(key)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, raw[:12], raw[12:], aad)
	return string(plain), err
}

// EncryptPost returns an encrypted copy, leaving the plaintext post intact.
func EncryptPost(p *Post, key []byte, group string, epoch int, random io.Reader) (*Post, error) {
	if p == nil {
		return nil, errors.New("feed: nil post")
	}
	out := *p
	var err error
	out.Body, err = EncryptField(strings.TrimSpace(p.Body), key, group, epoch, "body", random)
	if err != nil {
		return nil, err
	}
	out.Headline, err = EncryptField(p.Headline, key, group, epoch, "headline", random)
	if err != nil {
		return nil, err
	}
	out.Dek, err = EncryptField(p.Dek, key, group, epoch, "dek", random)
	if err != nil {
		return nil, err
	}
	out.Group, out.Epoch, out.Enc = group, epoch, "v1"
	return &out, nil
}

func DecryptPost(p *Post, key []byte) (*Post, error) {
	if p == nil {
		return nil, errors.New("feed: nil post")
	}
	if p.Enc != "v1" {
		return nil, errors.New("feed: post is not enc:v1")
	}
	out := *p
	var err error
	out.Body, err = DecryptField(p.Body, key, p.Group, p.Epoch, "body")
	if err != nil {
		return nil, err
	}
	out.Headline, err = DecryptField(p.Headline, key, p.Group, p.Epoch, "headline")
	if err != nil {
		return nil, err
	}
	out.Dek, err = DecryptField(p.Dek, key, p.Group, p.Epoch, "dek")
	if err != nil {
		return nil, err
	}
	return &out, nil
}
