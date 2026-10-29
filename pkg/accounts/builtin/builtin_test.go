package builtin

import (
	"testing"

	"github.com/qiangli/yoke/pkg/accounts"
	"github.com/qiangli/yoke/pkg/accounts/accountstest"
)

// Linking builtin must put every built-in Discover-capable adapter,
// including cloudbox and llm, into the default registry exactly once.
func TestBuiltinAdaptersRegistered(t *testing.T) {
	r := accounts.DefaultRegistry()
	seen := map[string]bool{}
	for _, a := range r.Adapters(accounts.Discover) {
		seen[a.Name()] = true
	}
	for _, want := range []string{"cloudbox", "llm"} {
		if !seen[want] {
			t.Fatalf("default registry missing %q (discoverable: %v)", want, r.Adapters(accounts.Discover))
		}
	}
	for _, name := range []string{"cloudbox", "llm"} {
		a, err := r.Adapter(name, accounts.Discover)
		if err != nil {
			t.Fatalf("adapter %q: %v", name, err)
		}
		accountstest.AssertNoSecret(t, a.Name())
	}
}
