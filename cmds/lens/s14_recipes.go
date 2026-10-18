package lenscmd

import (
	"context"
	"fmt"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func init() {
	RegisterProvider("oci", func(_ *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewOCIRecipe(nil), nil
	})
	RegisterProvider("k8s", k8sProvider)
	RegisterProvider("cloud", cloudProvider)
}

// k8sProvider serves k8s:pods@k8s:<context> through the read-only pods
// recipe. An unscoped locator or any other scope fails closed: there is no
// ambient kubectl-context fallback, matching cloudProvider.
func k8sProvider(_ *tool.RunContext, loc ref.Locator) (provider.Provider, error) {
	if loc.Scheme != "k8s" || loc.ScopeKind != "k8s" {
		return nil, fmt.Errorf("k8s pods need a @k8s scope, got %q", loc.String())
	}
	return provider.NewK8sScopedRecipe(loc.ScopeValue, k8sRunner)
}

// cloudRunner is nil in production (the real read-only runner); tests inject one.
var cloudRunner func(context.Context, string, ...string) ([]byte, error)

// cloudProvider serves cloud:droplets@cloud:<context> through the read-only
// droplets recipe. An unscoped locator or any other scope fails closed: there
// is no ambient DIGITALOCEAN_CONTEXT fallback.
func cloudProvider(_ *tool.RunContext, loc ref.Locator) (provider.Provider, error) {
	if loc.Scheme != "cloud" || loc.ScopeKind != "cloud" {
		return nil, fmt.Errorf("cloud droplets need a @cloud scope, got %q", loc.String())
	}
	return provider.NewCloudDropletsRecipe(loc.ScopeValue, cloudRunner)
}
