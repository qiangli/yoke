package accounts

import (
	"errors"
	"testing"
)

func TestValidCredentialRef(t *testing.T) {
	tests := []struct {
		name  string
		ref   string
		valid bool
	}{
		// Real-shaped OpenSSH fingerprints: SHA256 with base64 (+, /, optional = padding)
		{
			name:  "sha256 standard unpadded",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg",
			valid: true,
		},
		{
			name:  "sha256 single padding",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg=",
			valid: true,
		},
		{
			name:  "sha256 double padding",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg==",
			valid: true,
		},
		{
			name:  "sha256 another real fingerprint",
			ref:   "ssh-key:SHA256:p2x+k4/QvM8eG5RtY7U1I3O9P0A2S4D6F8H+J1K3L5M",
			valid: true,
		},

		// Real-shaped OpenSSH fingerprints: MD5 with colons
		{
			name:  "md5 lowercase",
			ref:   "ssh-key:MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99",
			valid: true,
		},
		{
			name:  "md5 uppercase",
			ref:   "ssh-key:MD5:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99",
			valid: true,
		},

		// Existing valid named ssh-key and other prefixes
		{
			name:  "ssh-key named",
			ref:   "ssh-key:id_ed25519",
			valid: true,
		},
		{
			name:  "ssh-key hyphen and dot",
			ref:   "ssh-key:id_rsa-2024.pub",
			valid: true,
		},
		{
			name:  "secret reference",
			ref:   "secret:prod-db-password",
			valid: true,
		},
		{
			name:  "env reference",
			ref:   "env:MY_AUTH_TOKEN_2",
			valid: true,
		},
		{
			name:  "profile reference",
			ref:   "profile:staging.east",
			valid: true,
		},

		// Rejects: spaces
		{
			name:  "space internal",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg foo",
			valid: false,
		},
		{
			name:  "space after prefix",
			ref:   "ssh-key: SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg",
			valid: false,
		},
		{
			name:  "space leading",
			ref:   " ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg",
			valid: false,
		},
		{
			name:  "space trailing",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg ",
			valid: false,
		},

		// Rejects: newlines
		{
			name:  "newline internal",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4Xo\nY9zs4+AoWome8V4/m5jxg",
			valid: false,
		},
		{
			name:  "crlf internal",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4Xo\r\nY9zs4+AoWome8V4/m5jxg",
			valid: false,
		},
		{
			name:  "newline trailing",
			ref:   "ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg\n",
			valid: false,
		},

		// Rejects: empty
		{
			name:  "empty string",
			ref:   "",
			valid: false,
		},
		{
			name:  "empty ssh-key target",
			ref:   "ssh-key:",
			valid: false,
		},
		{
			name:  "empty secret target",
			ref:   "secret:",
			valid: false,
		},

		// Rejects: malformed prefix or characters
		{
			name:  "ssh-key bare",
			ref:   "ssh-key",
			valid: false,
		},
		{
			name:  "unknown scheme",
			ref:   "token:abc123xyz",
			valid: false,
		},
		{
			name:  "tab character",
			ref:   "ssh-key:SHA256:abc\tdef",
			valid: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidCredentialRef(tc.ref)
			if got != tc.valid {
				t.Fatalf("ValidCredentialRef(%q) = %v, want %v", tc.ref, got, tc.valid)
			}
		})
	}
}

func TestDuplicateIdentityError(t *testing.T) {
	withAlias := &DuplicateIdentityError{
		Existing: Account{
			ID:    "acc_1234567890ab",
			Alias: "owl",
		},
	}
	if got := withAlias.Message(); got != "identity already recorded as owl (acc_1234567890ab)" {
		t.Fatalf("Message() = %q", got)
	}
	if got := withAlias.Error(); got != "accounts: identity already recorded as owl (acc_1234567890ab)" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(withAlias, ErrDuplicateIdentity) {
		t.Fatal("errors.Is(withAlias, ErrDuplicateIdentity) = false")
	}

	withoutAlias := &DuplicateIdentityError{
		Existing: Account{
			ID: "acc_1234567890ab",
		},
	}
	if got := withoutAlias.Message(); got != "identity already recorded as acc_1234567890ab" {
		t.Fatalf("Message() = %q", got)
	}
	if got := withoutAlias.Error(); got != "accounts: identity already recorded as acc_1234567890ab" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(withoutAlias, ErrDuplicateIdentity) {
		t.Fatal("errors.Is(withoutAlias, ErrDuplicateIdentity) = false")
	}
}
