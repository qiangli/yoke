package guard

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// The vault matcher keeps digests only: the 0600 cache never holds a value,
// a stale cache is refreshed, and an unreachable vault falls back to the
// digests it already had.
func TestVaultCacheHoldsDigestsOnly(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	isolate(t, secret)
	if !loadVault(context.Background()).equal(secret) {
		t.Fatal("vault value not recognised")
	}
	path := vaultCachePath()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "battery") {
		t.Fatal("the vault cache holds a plaintext value")
	}

	// The vault becomes unreachable: the cached digests still match.
	resetVault()
	vaultValues = func(context.Context, bool, func(string)) error { return errors.New("offline") }
	if !loadVault(context.Background()).equal(secret) {
		t.Fatal("cached digest lost when the vault was unreachable")
	}
	if ValueReason(context.Background(), "prefix "+secret+" suffix") != "value:vault" {
		t.Fatal("embedded vault value not refused")
	}
	if ValueReason(context.Background(), "correct-horse") != "" {
		t.Fatal("a fragment of a vault value was refused")
	}
}
