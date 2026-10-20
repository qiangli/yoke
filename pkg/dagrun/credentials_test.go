package dagrun_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dagrun"
)

type fakeCredentialSource struct {
	creds map[dagrun.CredentialRef]dagrun.Credential
	errs  map[dagrun.CredentialRef]error
}

func (f *fakeCredentialSource) Resolve(_ context.Context, ref dagrun.CredentialRef) (dagrun.Credential, error) {
	if err, ok := f.errs[ref]; ok {
		return dagrun.Credential{}, err
	}
	if cred, ok := f.creds[ref]; ok {
		return cred, nil
	}
	return dagrun.Credential{}, &dagrun.ErrCredentialUnknown{
		Ref:    ref,
		Reason: "not found in test fake source",
	}
}

// TestClusterCredentialNeverInArgvOrLogs verifies that a delivered credential's bytes
// never appear in the generated manifest, the argv, or redacted log/result text,
// and that a revoked credential is visibly refused.
func TestClusterCredentialNeverInArgvOrLogs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	const ref = dagrun.CredentialRef("anthropic/subscription")
	fakeSecretBytes := []byte("synthetic-fake-token-unit-0001")

	src := &fakeCredentialSource{
		creds: map[dagrun.CredentialRef]dagrun.Credential{
			ref: {
				Ref:       ref,
				Value:     fakeSecretBytes,
				ExpiresAt: now.Add(24 * time.Hour),
				Revoked:   false,
			},
		},
	}

	// 1. Delivery resolves successfully out-of-band.
	bundle, err := dagrun.Delivery(ctx, []dagrun.CredentialRef{ref}, src, now)
	if err != nil {
		t.Fatalf("Delivery unexpected error: %v", err)
	}
	if bundle == nil {
		t.Fatal("Delivery returned nil bundle")
	}

	// Env contains candidate names mapped to secret value.
	envVal, ok := bundle.Env["ANTHROPIC_SUBSCRIPTION_API_KEY"]
	if !ok || envVal != string(fakeSecretBytes) {
		t.Errorf("expected bundle.Env to contain ANTHROPIC_SUBSCRIPTION_API_KEY, got %v", bundle.Env)
	}

	// 2. Secret value never in generated manifest or argv.
	argv := []string{"claude", "eval", "--suite", "smoke"}
	in := dagrun.GenerateInput{
		Command: dagrun.CommandRef{
			Name:   "claude",
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		Argv:        argv,
		WorkspaceID: "ws-01jb6z00000000000000000000",
		RunID:       "run-01jb7c00000000000000000000",
		Venue:       "cluster",
		Run: dagrun.RunSpec{
			Credentials: []string{string(ref)},
		},
	}

	manifestBytes, err := dagrun.Generate(in)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if bytes.Contains(manifestBytes, fakeSecretBytes) {
		t.Fatal("CRITICAL: delivered credential value leaked into generated manifest bytes")
	}
	for _, arg := range argv {
		if strings.Contains(arg, string(fakeSecretBytes)) {
			t.Fatal("CRITICAL: credential value present in argv")
		}
	}
	if !bytes.Contains(manifestBytes, []byte("Secrets: anthropic/subscription")) {
		t.Fatal("manifest missing Secrets: reference line")
	}

	// 3. Secret value is masked from log and RunRecord text before persistence.
	rawLog := []byte("2026-10-20T12:01:00Z auth bearer token " + string(fakeSecretBytes) + " call completed")
	redacted := bundle.Redact(rawLog)
	if bytes.Contains(redacted, fakeSecretBytes) {
		t.Fatal("CRITICAL: Redact leaked delivered credential value into logs")
	}
	if !bytes.Contains(redacted, []byte("[redacted:anthropic_subscription]")) {
		t.Errorf("expected [redacted:anthropic_subscription] replacement, got: %s", string(redacted))
	}

	// 4. Revoked credential is refused visibly.
	revokedRef := dagrun.CredentialRef("anthropic/revoked-subscription")
	src.creds[revokedRef] = dagrun.Credential{
		Ref:       revokedRef,
		Value:     []byte("synthetic-revoked-token-unit-0002"),
		ExpiresAt: now.Add(24 * time.Hour),
		Revoked:   true,
	}

	_, err = dagrun.Delivery(ctx, []dagrun.CredentialRef{revokedRef}, src, now)
	if err == nil {
		t.Fatal("Delivery expected error for revoked credential, got nil")
	}
	var revErr *dagrun.ErrCredentialRevoked
	if !errors.As(err, &revErr) {
		t.Fatalf("Delivery returned error %T (%v), want *ErrCredentialRevoked", err, err)
	}
	if revErr.Ref != revokedRef {
		t.Errorf("ErrCredentialRevoked ref = %q, want %q", revErr.Ref, revokedRef)
	}
}

// TestCredentialExpiredDeniesWithVisibleReason verifies that an expired subscription
// login is denied with ErrCredentialExpired carrying an actionable visible reason,
// and no fallback occurs.
func TestCredentialExpiredDeniesWithVisibleReason(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	const ref = dagrun.CredentialRef("anthropic/subscription")
	expiry := now.Add(-10 * time.Minute)

	src := &fakeCredentialSource{
		creds: map[dagrun.CredentialRef]dagrun.Credential{
			ref: {
				Ref:       ref,
				Value:     []byte("synthetic-expired-token-unit-0003"),
				ExpiresAt: expiry,
				Revoked:   false,
			},
		},
	}

	bundle, err := dagrun.Delivery(ctx, []dagrun.CredentialRef{ref}, src, now)
	if bundle != nil {
		t.Fatal("Delivery returned bundle for expired credential, want nil")
	}
	if err == nil {
		t.Fatal("Delivery expected ErrCredentialExpired, got nil")
	}

	var expErr *dagrun.ErrCredentialExpired
	if !errors.As(err, &expErr) {
		t.Fatalf("Delivery returned %T (%v), want *ErrCredentialExpired", err, err)
	}
	if expErr.Ref != ref {
		t.Errorf("expired error ref = %q, want %q", expErr.Ref, ref)
	}
	if !expErr.ExpiresAt.Equal(expiry) {
		t.Errorf("expired error ExpiresAt = %v, want %v", expErr.ExpiresAt, expiry)
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "expired") {
		t.Errorf("error message missing 'expired': %q", errMsg)
	}
	if !strings.Contains(errMsg, string(ref)) {
		t.Errorf("error message missing ref %q: %q", ref, errMsg)
	}
	if !strings.Contains(errMsg, "fallback") && !strings.Contains(errMsg, "metered") && expErr.Reason == "" {
		t.Errorf("error message missing actionable reason: %q", errMsg)
	}
}

// TestValidateRefsRejectsSecretValues tests that ValidateRefs accepts valid vault references
// and rejects anything that looks like a secret VALUE (using shape detection) or is malformed.
func TestValidateRefsRejectsSecretValues(t *testing.T) {
	// Valid refs should pass
	valid := []dagrun.CredentialRef{
		"anthropic/subscription",
		"openai/fleet-shared",
		"custom-key",
		"vault:dept/team/secret",
	}
	if err := dagrun.ValidateRefs(valid); err != nil {
		t.Fatalf("ValidateRefs rejected valid refs: %v", err)
	}
	// Also test with string slice
	validStrings := []string{"anthropic/subscription", "provider/token"}
	if err := dagrun.ValidateRefs(validStrings); err != nil {
		t.Fatalf("ValidateRefs rejected valid strings: %v", err)
	}

	// Secret-value shapes must be rejected
	secretValues := []struct {
		name  string
		token string
	}{
		{"openai_key_shape", "sk-proj-obviouslyfakesecretkey01234567890abcdef"},
		{"github_token_shape", "ghp_obviouslyfaketoken1234567890abcdef12345678"},
		{"aws_access_key_shape", "AKIAIOSFODNN7EXAMPLE"},
		{"private_key_shape", "-----BEGIN RSA PRIVATE KEY-----"},
	}

	for _, tc := range secretValues {
		t.Run(tc.name, func(t *testing.T) {
			err := dagrun.ValidateRefs([]dagrun.CredentialRef{dagrun.CredentialRef(tc.token)})
			if err == nil {
				t.Fatalf("ValidateRefs accepted secret-shaped value %q", tc.name)
			}
			if !errors.Is(err, dagrun.ErrInvalidCredentialRef) {
				t.Errorf("error %v does not wrap ErrInvalidCredentialRef", err)
			}
		})
	}

	// Malformed refs must be rejected
	malformed := []struct {
		name string
		ref  string
	}{
		{"empty", ""},
		{"whitespace", "anthropic subscription"},
		{"comma", "anthropic,subscription"},
		{"newline", "anthropic\nsubscription"},
		{"assignment", "ANTHROPIC_KEY=fake-val"},
		{"expansion", "anthropic/${SECRET}"},
	}

	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			err := dagrun.ValidateRefs([]string{tc.ref})
			if err == nil {
				t.Fatalf("ValidateRefs accepted malformed ref %q (%s)", tc.ref, tc.name)
			}
			if !errors.Is(err, dagrun.ErrInvalidCredentialRef) {
				t.Errorf("error %v does not wrap ErrInvalidCredentialRef", err)
			}
		})
	}
}

// TestCredentialErrorsNeverContainValue verifies that credential errors never contain
// the secret value in their text or fields.
func TestCredentialErrorsNeverContainValue(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	const secretVal = "super-secret-canary-never-leak-9988776655"
	const ref = dagrun.CredentialRef("anthropic/subscription")

	// 1. ValidateRefs error with secret-shaped value
	secretShaped := "sk-proj-" + secretVal
	valErr := dagrun.ValidateRefs([]string{secretShaped})
	if valErr == nil {
		t.Fatal("ValidateRefs expected error, got nil")
	}
	if strings.Contains(valErr.Error(), secretVal) {
		t.Fatalf("CRITICAL: ValidateRefs error leaked secret value: %s", valErr.Error())
	}

	// 2. Revoked error
	srcRevoked := &fakeCredentialSource{
		creds: map[dagrun.CredentialRef]dagrun.Credential{
			ref: {
				Ref:       ref,
				Value:     []byte(secretVal),
				ExpiresAt: now.Add(time.Hour),
				Revoked:   true,
			},
		},
	}
	_, revErr := dagrun.Delivery(ctx, []dagrun.CredentialRef{ref}, srcRevoked, now)
	if revErr == nil {
		t.Fatal("Delivery expected error, got nil")
	}
	if strings.Contains(revErr.Error(), secretVal) {
		t.Fatalf("CRITICAL: ErrCredentialRevoked error leaked secret value: %s", revErr.Error())
	}

	// 3. Expired error
	srcExpired := &fakeCredentialSource{
		creds: map[dagrun.CredentialRef]dagrun.Credential{
			ref: {
				Ref:       ref,
				Value:     []byte(secretVal),
				ExpiresAt: now.Add(-time.Hour),
				Revoked:   false,
			},
		},
	}
	_, expErr := dagrun.Delivery(ctx, []dagrun.CredentialRef{ref}, srcExpired, now)
	if expErr == nil {
		t.Fatal("Delivery expected error, got nil")
	}
	if strings.Contains(expErr.Error(), secretVal) {
		t.Fatalf("CRITICAL: ErrCredentialExpired error leaked secret value: %s", expErr.Error())
	}

	// 4. Unknown error
	srcUnknown := &fakeCredentialSource{
		creds: map[dagrun.CredentialRef]dagrun.Credential{},
	}
	_, unkErr := dagrun.Delivery(ctx, []dagrun.CredentialRef{ref}, srcUnknown, now)
	if unkErr == nil {
		t.Fatal("Delivery expected error, got nil")
	}
	if strings.Contains(unkErr.Error(), secretVal) {
		t.Fatalf("CRITICAL: ErrCredentialUnknown error leaked secret value: %s", unkErr.Error())
	}
}

// TestDeliveryRedactsShapeMatchedSecrets verifies that DeliveryBundle.Redact
// also masks credential-shaped text that wasn't explicitly registered as a delivered value.
func TestDeliveryRedactsShapeMatchedSecrets(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	bundle, err := dagrun.Delivery(ctx, nil, nil, now)
	if err != nil {
		t.Fatalf("Delivery with nil refs returned error: %v", err)
	}

	input := []byte("found unmanaged key ghp_obviouslyfaketoken1234567890abcdef12345678 in logs")
	redacted := bundle.Redact(input)
	if bytes.Contains(redacted, []byte("ghp_obviouslyfaketoken1234567890abcdef12345678")) {
		t.Errorf("Redact failed to mask shape-matched secret: %s", string(redacted))
	}
}
