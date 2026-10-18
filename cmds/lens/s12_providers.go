package lenscmd

import (
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// S12 container and repo providers: tar:/zip:/gzip: read archives in place
// (chained with !/) and git:REV:PATH reads the object store, never a checkout.
func init() {
	for _, scheme := range []string{"tar", "zip", "gzip"} {
		RegisterProvider(scheme, archiveProvider)
	}
	RegisterProvider("git", gitProvider)
}

func archiveProvider(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
	return provider.NewArchive(rc.Path(".")), nil
}

func gitProvider(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
	return provider.NewGit(rc.Path(".")), nil
}
