package feed

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixedBytes(start, end byte) []byte {
	b := make([]byte, int(end-start)+1)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}
func hx(b []byte) string { return hex.EncodeToString(b) }
func vectorJSON(t *testing.T) []byte {
	t.Helper()
	memberRaw := fixedBytes(0x01, 0x20)
	ephRaw := fixedBytes(0x21, 0x40)
	groupKey := fixedBytes(0x41, 0x60)
	wrapNonce := fixedBytes(0x61, 0x6c)
	fieldNonce := fixedBytes(0x71, 0x7c)
	member, err := ecdh.X25519().NewPrivateKey(memberRaw)
	if err != nil {
		t.Fatal(err)
	}
	eph, err := ecdh.X25519().NewPrivateKey(ephRaw)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := eph.ECDH(member.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	salt := append(append([]byte{}, eph.PublicKey().Bytes()...), member.PublicKey().Bytes()...)
	info, _ := wrapInfo("g-test", 1)
	kek, err := deriveKEK(shared, eph.PublicKey().Bytes(), member.PublicKey().Bytes(), "g-test", 1)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := Wrap(groupKey, EncPubkeyB64(member), "g-test", 1, bytes.NewReader(append(append([]byte{}, ephRaw...), wrapNonce...)))
	if err != nil {
		t.Fatal(err)
	}
	wrappedRaw, _ := base64.StdEncoding.DecodeString(wrapped)
	field, err := EncryptField("hello group", groupKey, "g-test", 1, "body", bytes.NewReader(fieldNonce))
	if err != nil {
		t.Fatal(err)
	}
	fieldRaw, _ := base64.StdEncoding.DecodeString(field[len("enc:v1:"):])
	fieldAAD, _ := fieldAAD("g-test", 1, "body")
	data := struct {
		MemberPrivateHex    string `json:"member_private_hex"`
		MemberPublicHex     string `json:"member_public_hex"`
		EphemeralPrivateHex string `json:"ephemeral_private_hex"`
		EphemeralPublicHex  string `json:"ephemeral_public_hex"`
		SharedHex           string `json:"shared_hex"`
		SaltHex             string `json:"salt_hex"`
		InfoHex             string `json:"info_hex"`
		KekHex              string `json:"kek_hex"`
		GroupKeyHex         string `json:"group_key_hex"`
		WrapNonceHex        string `json:"wrap_nonce_hex"`
		WrapAADHex          string `json:"wrap_aad_hex"`
		WrapCiphertextHex   string `json:"wrap_ciphertext_hex"`
		WrappedRawHex       string `json:"wrapped_raw_hex"`
		Wrapped             string `json:"wrapped"`
		FieldAADHex         string `json:"field_aad_hex"`
		FieldNonceHex       string `json:"field_nonce_hex"`
		FieldCiphertextHex  string `json:"field_ciphertext_hex"`
		FieldRawHex         string `json:"field_raw_hex"`
		EncryptedFieldHex   string `json:"encrypted_field_hex"`
		EncryptedField      string `json:"encrypted_field"`
	}{hx(memberRaw), hx(member.PublicKey().Bytes()), hx(ephRaw), hx(eph.PublicKey().Bytes()), hx(shared), hx(salt), hx(info), hx(kek), hx(groupKey), hx(wrapNonce), hx([]byte(wrapTag)), hx(wrappedRaw[44:]), hx(wrappedRaw), wrapped, hx(fieldAAD), hx(fieldNonce), hx(fieldRaw[12:]), hx(fieldRaw), hx([]byte(field)), field}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}
func TestGroupVectors(t *testing.T) {
	want := vectorJSON(t)
	path := filepath.Join("testdata", "group_vectors.json")
	if os.Getenv("UPDATE_GROUP_VECTORS") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("group vector mismatch\n got: %s\nwant: %s", got, want)
	}
}
func TestGroupCryptoRoundTripRotationAndSignature(t *testing.T) {
	memberA, _ := ecdh.X25519().NewPrivateKey(fixedBytes(1, 32))
	memberB, _ := ecdh.X25519().NewPrivateKey(fixedBytes(33, 64))
	key1 := fixedBytes(65, 96)
	for _, m := range []*ecdh.PrivateKey{memberA, memberB} {
		wrapped, err := Wrap(key1, EncPubkeyB64(m), "g-test", 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Unwrap(wrapped, m, "g-test", 1)
		if err != nil || !bytes.Equal(got, key1) {
			t.Fatalf("unwrap: %v", err)
		}
		if _, err := Unwrap(wrapped, m, "other", 1); err == nil {
			t.Fatal("group tamper accepted")
		}
		if _, err := Unwrap(wrapped, m, "g-test", 2); err == nil {
			t.Fatal("epoch tamper accepted")
		}
	}
	original := &Post{Body: "  hello group  ", Headline: "headline", Dek: "dek"}
	enc, err := EncryptPost(original, key1, "g-test", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if original.Enc != "" || enc.Body == original.Body || enc.Headline == original.Headline {
		t.Fatal("post not encrypted as copy")
	}
	signingKey := ed25519.NewKeyFromSeed(fixedBytes(1, 32))
	if err := SignPost(signingKey, enc); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPost(enc); err != nil {
		t.Fatalf("ciphertext signature: %v", err)
	}
	plain, err := DecryptPost(enc, key1)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Body != "hello group" || plain.Headline != "headline" || plain.Dek != "dek" {
		t.Fatal("round trip mismatch")
	}
	changed := *enc
	changed.Group = "else"
	if _, err := DecryptPost(&changed, key1); err == nil {
		t.Fatal("group tamper accepted")
	}
	changed = *enc
	changed.Epoch = 2
	if _, err := DecryptPost(&changed, key1); err == nil {
		t.Fatal("epoch tamper accepted")
	}
	changed = *enc
	changed.Body = "enc:v1:" + base64.StdEncoding.EncodeToString(sha256.New().Sum(nil))
	if err := VerifyPost(&changed); err == nil {
		t.Fatal("changed ciphertext signature accepted")
	}
	key2 := fixedBytes(97, 128)
	wrapped2, err := Wrap(key2, EncPubkeyB64(memberA), "g-test", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unwrap(wrapped2, memberB, "g-test", 2); err == nil {
		t.Fatal("removed member unwrapped epoch 2")
	}
	post2, err := EncryptPost(original, key2, "g-test", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptPost(post2, key1); err == nil {
		t.Fatal("removed member decrypted epoch 2")
	}
}
func TestEncKeyFileAndGroupCache(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateEncKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateEncKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Bytes(), b.Bytes()) {
		t.Fatal("encryption key changed")
	}
	info, err := os.Stat(filepath.Join(dir, "x25519.key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", info.Mode().Perm())
	}
	key := fixedBytes(1, 32)
	if err := SaveGroupKey(dir, "g-test", 1, key); err != nil {
		t.Fatal(err)
	}
	got, err := LoadGroupKey(dir, "g-test", 1)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("cached group key: %v", err)
	}
	info, err = os.Stat(filepath.Join(dir, "groups", "g-test.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode %o", info.Mode().Perm())
	}
	if err := SaveGroupKey(dir, "../escape", 1, key); err == nil {
		t.Fatal("path traversal accepted")
	}
}
