package lenscmd

import (
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func init() {
	RegisterProvider("env", envProvider)
}

func envProvider(rc *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
	if rc != nil && rc.Env != nil {
		return provider.NewEnvWithEnviron(rc.Env), nil
	}
	return provider.NewEnv(), nil
}
