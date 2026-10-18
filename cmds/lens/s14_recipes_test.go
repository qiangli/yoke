package lenscmd

import "testing"

func TestPlatformRecipesRegistered(t *testing.T) {
	for _, scheme := range []string{"oci", "k8s"} {
		if providers[scheme] == nil {
			t.Errorf("%s provider is not registered", scheme)
		}
	}
}
