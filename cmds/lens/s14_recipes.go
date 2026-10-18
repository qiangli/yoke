package lenscmd

import (
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func init() {
	RegisterProvider("oci", func(_ *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewOCIRecipe(nil), nil
	})
	RegisterProvider("k8s", func(_ *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewK8sRecipe(nil), nil
	})
}
