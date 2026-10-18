package registry

import (
	"context"

	"github.com/qiangli/yoke/external/az"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// az — the Microsoft Azure CLI (Azure/azure-cli, MIT), tier 6 (cloud).
// Host-first like gcloud: the CLI's vendor installer is the trusted,
// self-updating path, so the host's own az is used and provisioning
// refuses with an actionable vendor-install hint instead of
// TOFU-downloading. PreferHost also exempts the entry from the
// default-registry pin gate, which exists for silently-downloaded tools,
// not host-first ones. See external/az for the full admission record.
func init() {
	register(Entry{
		Name:       "az",
		Tier:       6,
		License:    az.License,
		Synopsis:   "Azure CLI (managed external, MIT; host-first)",
		PreferHost: true,
		Long: `az (Microsoft Azure CLI, Azure/azure-cli, MIT) fronts resource groups,
VMs and ARM deployments with an explicit subscription
(AZURE_SUBSCRIPTION_ID) plus a credential source. bashy uses the host's
own install when present; if absent it points at the vendor installer
rather than downloading. All args pass through to az. There is no
ambient-subscription fallback and login never implies provisioning
permission. Deployment mutations preview with --what-if; other dry runs
refuse without execution.`,
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			return az.Resolve(ctx, version)
		},
	})
}
