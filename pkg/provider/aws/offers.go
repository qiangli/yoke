// Offer normalization for OpCapacity (S451.2 shape, S522.2 adapter).
//
// The capacity result's Data carries an "offers" array of provider.Offer
// next to the raw Lightsail catalog. Two things are explicit here rather
// than left unknown:
//
//   - ARCH. Lightsail bundles carry no architecture field, but every
//     Lightsail plan is x86_64 — there is no ARM Lightsail bundle. The
//     satellite installer ships amd64 only, so saying so plainly is what
//     lets a caller match a bundle to the artifact without guessing.
//   - LIFECYCLE. A Lightsail plan is billed by the MONTH and keeps billing
//     while the instance is stopped, which is different from a per-hour
//     provider and is the single most expensive thing to get wrong.
package aws

import (
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
)

// ArchX8664 is the only architecture Lightsail offers.
const ArchX8664 = "x86_64"

// offers normalizes the bundle catalog into provider offers for this
// adapter's region. A GPU plan is reported but marked unqualified: the
// catalog stays complete, and the refusal is a qualification note rather
// than a hidden row.
func (a *adapter) offers(bundles []bundle) []provider.Offer {
	at := a.now().UTC()
	out := make([]provider.Offer, 0, len(bundles))
	for _, b := range bundles {
		o := provider.Offer{
			Region:      a.cfg.Region,
			SKU:         b.BundleID,
			Purchase:    provider.PurchaseOnDemand,
			CPU:         provider.CPU{Arch: ArchX8664, Count: b.CPUCount},
			MemoryBytes: int64(b.RAMSizeInGb * float64(provider.GiB)),
			Storage: []provider.Storage{{
				Kind: "boot", SizeBytes: int64(b.DiskSizeInGb) * provider.GiB, Persistent: true,
			}},
			Network: provider.Network{
				Known:         b.TransferPerMonthInGb > 0,
				TransferBytes: int64(b.TransferPerMonthInGb) * provider.GiB,
			},
			Lifecycle: provider.Lifecycle{
				StopKeepsBilling: true, StopKeepsDisk: true, ReleaseDeletes: true,
				Note: "a Lightsail plan is billed monthly and keeps billing while the instance is stopped; destroy releases the instance and its boot disk, but an unattached static IP is billed separately",
			},
			Qualification: provider.Qualification{Qualified: true},
		}
		if b.Price > 0 {
			o.Price = provider.Price{Known: true, USDPerMonth: b.Price, USDPerHour: hourly(b.Price), ObservedAt: at}
		}
		o.Availability = provider.Availability{State: provider.AvailAvailable, ObservedAt: at}
		if !b.IsActive {
			o.Availability = provider.Availability{State: provider.AvailUnavailable,
				Reason: "bundle is not active in region " + a.cfg.Region, ObservedAt: at}
		}
		switch {
		case isGPUBundle(b):
			o.Qualification = provider.Qualification{Reason: "GPU plans are never provisioned by this adapter"}
		case len(b.SupportedPlatforms) > 0 && !sliceHasFold(b.SupportedPlatforms, "LINUX_UNIX"):
			o.Qualification = provider.Qualification{Reason: "bundle does not support LINUX_UNIX blueprints"}
		case b.Price > a.cfg.Budget.perInstance():
			o.Qualification = provider.Qualification{Reason: "over the per-instance monthly ceiling"}
		}
		out = append(out, o)
	}
	return out
}

func sliceHasFold(in []string, want string) bool {
	for _, s := range in {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
