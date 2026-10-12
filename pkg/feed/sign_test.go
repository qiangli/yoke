package feed

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

type payloadFields struct {
	bodySHA256 string
	headline   string
	dek        string
	labels     []string
	replyTo    string
}

func (f payloadFields) payload() []byte {
	return SigningPayload(f.bodySHA256, f.headline, f.dek, f.labels, f.replyTo)
}

func baseFields() payloadFields {
	return payloadFields{
		bodySHA256: "9f86d081884c7d65",
		headline:   "Fed holds rates",
		dek:        "What it means for markets",
		labels:     []string{"fed", "macro"},
		replyTo:    "re:feed-0001",
	}
}

func TestSigningPayloadDeterministic(t *testing.T) {
	a := baseFields().payload()
	b := baseFields().payload()
	if !bytes.Equal(a, b) {
		t.Fatal("SigningPayload is not deterministic for identical inputs")
	}
	if !bytes.HasPrefix(a, []byte("dhnt-feed-sig-v1\n")) {
		t.Fatalf("payload missing version tag line, got %q", a[:20])
	}
}

func TestSigningPayloadLabelOrderPreserved(t *testing.T) {
	f := baseFields()
	first := SigningPayload(f.bodySHA256, f.headline, f.dek, []string{"macro", "fed"}, f.replyTo)
	if bytes.Equal(first, f.payload()) {
		t.Fatal("SigningPayload sorted or ignored label order")
	}
}

func TestSigningPayloadFieldBoundaryShift(t *testing.T) {
	shifted := SigningPayload("h", "ab", "c", nil, "")
	other := SigningPayload("h", "a", "bc", nil, "")
	if bytes.Equal(shifted, other) {
		t.Fatal("SigningPayload is ambiguous across the headline/dek boundary")
	}
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	good := baseFields().payload()
	pub, sig := SignPayload(priv, good)
	if err := VerifyPayload(pub, sig, good); err != nil {
		t.Fatalf("verify round trip: %v", err)
	}
	if _, err := base64.StdEncoding.DecodeString(pub); err != nil {
		t.Fatalf("pub not std base64: %v", err)
	}
	if _, err := base64.StdEncoding.DecodeString(sig); err != nil {
		t.Fatalf("sig not std base64: %v", err)
	}
}

func TestTamperedFieldsFailVerification(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	base := baseFields()
	good := base.payload()
	pub, sig := SignPayload(priv, good)

	variants := []struct {
		name   string
		fields payloadFields
	}{
		{
			name:   "bodySHA256 changed",
			fields: payloadFields{base.bodySHA256 + "x", base.headline, base.dek, base.labels, base.replyTo},
		},
		{
			name:   "headline changed",
			fields: payloadFields{base.bodySHA256, base.headline + "!", base.dek, base.labels, base.replyTo},
		},
		{
			name:   "dek changed",
			fields: payloadFields{base.bodySHA256, base.headline, base.dek + "?", base.labels, base.replyTo},
		},
		{
			name:   "label text changed",
			fields: payloadFields{base.bodySHA256, base.headline, base.dek, []string{"fed", "markets"}, base.replyTo},
		},
		{
			name:   "label order swapped",
			fields: payloadFields{base.bodySHA256, base.headline, base.dek, []string{"macro", "fed"}, base.replyTo},
		},
		{
			name:   "label removed",
			fields: payloadFields{base.bodySHA256, base.headline, base.dek, []string{"fed"}, base.replyTo},
		},
		{
			name:   "label added",
			fields: payloadFields{base.bodySHA256, base.headline, base.dek, []string{"fed", "macro", "hot"}, base.replyTo},
		},
		{
			name:   "labels dropped entirely",
			fields: payloadFields{base.bodySHA256, base.headline, base.dek, nil, base.replyTo},
		},
		{
			name:   "replyTo changed",
			fields: payloadFields{base.bodySHA256, base.headline, base.dek, base.labels, "re:feed-9999"},
		},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			tampered := v.fields.payload()
			if bytes.Equal(tampered, good) {
				t.Fatal("tampered variant produced identical payload")
			}
			if err := VerifyPayload(pub, sig, tampered); err == nil {
				t.Fatal("signature verified for tampered payload")
			}
		})
	}
}

func TestBoundaryShiftFailsVerification(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	abC := SigningPayload("h", "ab", "c", nil, "")
	aBc := SigningPayload("h", "a", "bc", nil, "")
	pub, sig := SignPayload(priv, abC)
	if err := VerifyPayload(pub, sig, aBc); err == nil {
		t.Fatal("signature carried across a field-boundary shift")
	}
}

func TestVerifyPayloadErrors(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	payload := baseFields().payload()
	pub, sig := SignPayload(priv, payload)

	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	otherPub, _ := SignPayload(other, payload)

	cases := []struct {
		name          string
		pubB64, sigB64 string
		wantSub       string
	}{
		{name: "pub not base64", pubB64: "not-base64!", sigB64: sig, wantSub: "base64"},
		{name: "pub wrong length", pubB64: base64.StdEncoding.EncodeToString([]byte("short")), sigB64: sig, wantSub: "wrong length"},
		{name: "sig not base64", pubB64: pub, sigB64: "not-base64!", wantSub: "base64"},
		{name: "sig wrong length", pubB64: pub, sigB64: base64.StdEncoding.EncodeToString(make([]byte, 10)), wantSub: "wrong length"},
		{name: "wrong pubkey", pubB64: otherPub, sigB64: sig, wantSub: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := VerifyPayload(c.pubB64, c.sigB64, payload)
			if err == nil {
				t.Fatal("VerifyPayload succeeded, want error")
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not contain %q", err, c.wantSub)
			}
		})
	}
}
