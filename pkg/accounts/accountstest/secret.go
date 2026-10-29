// Package accountstest supplies a shared secret canary for account surfaces.
package accountstest

import (
	"fmt"
	"strings"
	"testing"
)

const CanarySecret = "canary-secret-value-do-not-serialize-524"

func AssertNoSecret(t testing.TB, surfaces ...any) {
	t.Helper()
	for _, s := range surfaces {
		if strings.Contains(fmt.Sprint(s), CanarySecret) {
			t.Fatal("secret canary escaped")
		}
	}
}
