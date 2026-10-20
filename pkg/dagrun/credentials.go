package dagrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/secrets"
)

// ErrInvalidCredentialRef is returned when a credential reference is empty,
// malformed, or looks like a secret value rather than a vault reference.
var ErrInvalidCredentialRef = errors.New("dagrun: invalid credential reference")

// CredentialRef is a vault reference identifier (e.g. "anthropic/subscription"),
// never a secret value.
type CredentialRef string

// Credential represents a resolved credential from a vault or credential store.
type Credential struct {
	Ref       CredentialRef
	Value     []byte
	ExpiresAt time.Time
	Revoked   bool
}

// CredentialSource resolves vault credential references into delivered credentials.
type CredentialSource interface {
	Resolve(ctx context.Context, ref CredentialRef) (Credential, error)
}

// ErrCredentialRevoked indicates that the requested credential has been revoked.
// It carries the reference and an actionable reason, never the secret value.
type ErrCredentialRevoked struct {
	Ref    CredentialRef
	Reason string
}

func (e *ErrCredentialRevoked) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("credential %q is revoked: %s", e.Ref, e.Reason)
	}
	return fmt.Sprintf("credential %q is revoked", e.Ref)
}

func (e *ErrCredentialRevoked) Is(target error) bool {
	_, ok := target.(*ErrCredentialRevoked)
	return ok
}

// ErrCredentialExpired indicates that the credential's validity period has ended.
// It carries the reference, expiry timestamp, and an actionable reason, never the secret value.
type ErrCredentialExpired struct {
	Ref       CredentialRef
	ExpiresAt time.Time
	Reason    string
}

func (e *ErrCredentialExpired) Error() string {
	if !e.ExpiresAt.IsZero() {
		if e.Reason != "" {
			return fmt.Sprintf("credential %q expired at %s: %s", e.Ref, e.ExpiresAt.UTC().Format(time.RFC3339), e.Reason)
		}
		return fmt.Sprintf("credential %q expired at %s", e.Ref, e.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if e.Reason != "" {
		return fmt.Sprintf("credential %q expired: %s", e.Ref, e.Reason)
	}
	return fmt.Sprintf("credential %q expired", e.Ref)
}

func (e *ErrCredentialExpired) Is(target error) bool {
	_, ok := target.(*ErrCredentialExpired)
	return ok
}

// ErrCredentialUnknown indicates that the credential could not be found or resolved.
// It carries the reference and an actionable reason, never the secret value.
type ErrCredentialUnknown struct {
	Ref    CredentialRef
	Reason string
}

func (e *ErrCredentialUnknown) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("credential %q is unknown: %s", e.Ref, e.Reason)
	}
	return fmt.Sprintf("credential %q is unknown", e.Ref)
}

func (e *ErrCredentialUnknown) Is(target error) bool {
	_, ok := target.(*ErrCredentialUnknown)
	return ok
}

// ValidateRefs checks that every entry in refs is a valid vault reference
// and rejects anything that looks like a secret value or malformed token.
// Callers of Generate can run this to fail closed before manifest emission.
func ValidateRefs[T ~string](refs []T) error {
	for _, r := range refs {
		if err := ValidateRef(r); err != nil {
			return err
		}
	}
	return nil
}

// ValidateRef checks a single credential reference. It rejects secret values
// (using secrets.Redactor shape detection), whitespace, commas, controls,
// assignments, or empty references. The returned error never includes the
// rejected secret text.
func ValidateRef[T ~string](ref T) error {
	s := string(ref)
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%w: empty credential reference", ErrInvalidCredentialRef)
	}
	if !isListToken(s) {
		return fmt.Errorf("%w: credential reference contains whitespace, commas, control characters, or variable expansion", ErrInvalidCredentialRef)
	}
	if strings.Contains(s, "=") {
		return fmt.Errorf("%w: credential reference looks like an environment assignment", ErrInvalidCredentialRef)
	}

	matches := secrets.DetectCredentialShapes([]byte(s))
	if len(matches) > 0 {
		return fmt.Errorf("%w: credential reference rejected: looks like a secret value (%s shape detected)", ErrInvalidCredentialRef, matches[0].Kind)
	}
	return nil
}

// DeliveryBundle contains out-of-band credential environment variables for the executor,
// along with a redactor configured to mask delivered values and credential shapes
// from logs and RunRecords before persistence.
type DeliveryBundle struct {
	Env      map[string]string
	EnvList  []string
	redactor *secrets.Redactor
	values   [][]byte
	labels   []string
}

// Bundle is an alias for DeliveryBundle.
type Bundle = DeliveryBundle

// Environ returns the environment variable slice in NAME=VALUE form.
func (b *DeliveryBundle) Environ() []string {
	if b == nil {
		return nil
	}
	return append([]string(nil), b.EnvList...)
}

// Redact removes every delivered credential value and any credential-shaped
// text (such as OpenAI keys, GitHub tokens, AWS keys, or private keys) from
// log or RunRecord text before persistence.
func (b *DeliveryBundle) Redact(text []byte) []byte {
	if len(text) == 0 {
		return text
	}
	var out []byte
	if b != nil && b.redactor != nil {
		out = b.redactor.Redact(text)
	} else {
		r := secrets.NewRedactor()
		_ = r.SetShapeMode(secrets.ShapeMask)
		out = r.Redact(text)
	}

	// Direct byte-level replacement as guarantee for delivered secrets of any length
	if b != nil {
		for i, val := range b.values {
			if len(val) > 0 && bytes.Contains(out, val) {
				replacement := []byte("[redacted:" + b.labels[i] + "]")
				out = bytes.ReplaceAll(out, val, replacement)
			}
		}
	}
	return out
}

// Redact removes credential shapes and any provided literal secret values from
// text before persistence or output.
func Redact(text []byte, extraValues ...[]byte) []byte {
	if len(text) == 0 {
		return text
	}
	r := secrets.NewRedactor()
	_ = r.SetShapeMode(secrets.ShapeMask)
	out := r.Redact(text)
	for _, val := range extraValues {
		if len(val) > 0 && bytes.Contains(out, val) {
			out = bytes.ReplaceAll(out, val, []byte("[redacted]"))
		}
	}
	return out
}

// Delivery resolves vault credential references out-of-band for executor dispatch.
// It maps candidate environment variable names (from secrets.CredentialEnvNames) to values,
// keeping them OUT of argv and OUT of the manifest.
//
// Delivery fails closed if any reference is revoked, expired, unknown, or malformed.
// Expired subscription logins fail visibly with ErrCredentialExpired and never fall
// back to metered API keys.
func Delivery(ctx context.Context, refs []CredentialRef, src CredentialSource, now time.Time) (*DeliveryBundle, error) {
	if err := ValidateRefs(refs); err != nil {
		return nil, err
	}

	redactor := secrets.NewRedactor()
	_ = redactor.SetShapeMode(secrets.ShapeMask)

	bundle := &DeliveryBundle{
		Env:      make(map[string]string),
		redactor: redactor,
	}

	if len(refs) == 0 {
		return bundle, nil
	}
	if src == nil {
		return nil, &ErrCredentialUnknown{
			Ref:    CredentialRef(refs[0]),
			Reason: "no credential source provided",
		}
	}

	seen := make(map[CredentialRef]struct{}, len(refs))
	for _, r := range refs {
		cRef := CredentialRef(r)
		if _, ok := seen[cRef]; ok {
			continue
		}
		seen[cRef] = struct{}{}

		cred, err := src.Resolve(ctx, cRef)
		if err != nil {
			var (
				rev *ErrCredentialRevoked
				exp *ErrCredentialExpired
				unk *ErrCredentialUnknown
			)
			if errors.As(err, &rev) || errors.As(err, &exp) || errors.As(err, &unk) {
				return nil, err
			}
			return nil, &ErrCredentialUnknown{
				Ref:    cRef,
				Reason: fmt.Sprintf("credential resolution failed: %v", err),
			}
		}

		if cred.Revoked {
			return nil, &ErrCredentialRevoked{
				Ref:    cRef,
				Reason: "credential is marked revoked in vault",
			}
		}

		if !cred.ExpiresAt.IsZero() && !cred.ExpiresAt.After(now) {
			return nil, &ErrCredentialExpired{
				Ref:       cRef,
				ExpiresAt: cred.ExpiresAt,
				Reason:    "subscription credential expired; renew login (credential facilities do not fall back to metered keys)",
			}
		}

		if len(cred.Value) == 0 {
			return nil, &ErrCredentialUnknown{
				Ref:    cRef,
				Reason: "credential has empty value in vault",
			}
		}

		cleanName := cleanSecretName(string(cRef))
		bundle.values = append(bundle.values, cred.Value)
		bundle.labels = append(bundle.labels, cleanName)

		if len(cred.Value) >= secrets.MinRedactedValueLen {
			_ = redactor.Register(cleanName, string(cred.Value))
		}

		envNames := secrets.CredentialEnvNames(string(cRef))
		valStr := string(cred.Value)
		for _, name := range envNames {
			bundle.Env[name] = valStr
		}
	}

	for k, v := range bundle.Env {
		bundle.EnvList = append(bundle.EnvList, k+"="+v)
	}
	sort.Strings(bundle.EnvList)

	return bundle, nil
}

func cleanSecretName(ref string) string {
	var sb strings.Builder
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('_')
		}
	}
	s := sb.String()
	if s == "" {
		return "secret"
	}
	return s
}
