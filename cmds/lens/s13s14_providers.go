package lenscmd

import (
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func init() {
	RegisterProvider("json", func(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return &provider.DocumentProvider{Scheme: "json", Root: rc.Path(".")}, nil
	})
	RegisterProvider("yaml", func(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return &provider.DocumentProvider{Scheme: "yaml", Root: rc.Path(".")}, nil
	})
	RegisterProvider("ast", func(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewAST(rc.Path(".")), nil
	})
	RegisterProvider("md", func(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewMarkdown(rc.Path(".")), nil
	})
	RegisterProvider("pdf", func(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return &provider.PDF{Root: rc.Path(".")}, nil
	})
	RegisterProvider("graph", func(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewGraph(rc.Path(".")), nil
	})
	RegisterProvider("proc", func(_ *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewProc(), nil
	})
	RegisterProvider("net", func(_ *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
		return provider.NewNet(), nil
	})
}
