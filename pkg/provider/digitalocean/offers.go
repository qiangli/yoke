package digitalocean

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
)

// unqualifiedSpotModels are accelerators whose spot offers are reported but
// explicitly not qualified for v1. It is a qualification note on reported
// offers, not a catalog allowlist: every other SKU is still discovered.
var unqualifiedSpotModels = []string{"b300", "mi350x", "mi355x"}

type doRegion struct {
	Slug      string   `json:"slug"`
	Available bool     `json:"available"`
	Sizes     []string `json:"sizes"`
}

type doGPUDevice struct {
	Model    string  `json:"model"`
	Fraction float64 `json:"fraction"`
	Unified  bool    `json:"unified"`
	VRAM     *struct {
		Amount float64 `json:"amount"`
		Unit   string  `json:"unit"`
	} `json:"vram"`
}

type doDisk struct {
	Type string `json:"type"`
	Size *struct {
		Amount float64 `json:"amount"`
		Unit   string  `json:"unit"`
	} `json:"size"`
}

type doSize struct {
	Slug            string   `json:"slug"`
	VCPUs           int      `json:"vcpus"`
	Memory          int64    `json:"memory"`   // MiB
	Disk            int64    `json:"disk"`     // GiB
	Transfer        float64  `json:"transfer"` // TB
	Arch            string   `json:"arch"`
	DiskInfo        []doDisk `json:"disk_info"`
	PriceMonthly    *float64 `json:"price_monthly"`
	PriceHourly     *float64 `json:"price_hourly"`
	SpotPriceHourly *float64 `json:"spot_price_hourly"`
	Available       bool     `json:"available"`
	Regions         []string `json:"regions"`
	GPU             *struct {
		Count int `json:"count"`
		doGPUDevice
		Devices []doGPUDevice `json:"devices"`
	} `json:"gpu_info"`
}

// unitBytes converts an exact known unit to bytes; ok is false for any
// unit it does not know, so callers never guess a size.
func unitBytes(amount float64, unit string) (int64, bool) {
	var mult float64
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	case "kb":
		mult = 1e3
	case "mb":
		mult = 1e6
	case "gb":
		mult = 1e9
	case "tb":
		mult = 1e12
	default:
		return 0, false
	}
	return int64(amount * mult), true
}

func vramBytes(d doGPUDevice) int64 {
	if d.VRAM == nil {
		return 0
	}
	n, _ := unitBytes(d.VRAM.Amount, d.VRAM.Unit)
	return n
}

// storage prefers disk_info (boot/local persist; scratch and unknown types do
// not claim persistence) and falls back to the legacy disk size. Disks with an
// unknown size unit are omitted rather than guessed.
func (s doSize) storage() []provider.Storage {
	if len(s.DiskInfo) == 0 {
		return []provider.Storage{{Kind: "boot", SizeBytes: s.Disk * provider.GiB, Persistent: true}}
	}
	var out []provider.Storage
	for _, d := range s.DiskInfo {
		if d.Size == nil {
			continue
		}
		n, ok := unitBytes(d.Size.Amount, d.Size.Unit)
		if !ok {
			continue
		}
		kind := strings.ToLower(d.Type)
		out = append(out, provider.Storage{Kind: kind, SizeBytes: n, Persistent: kind == "boot" || kind == "local"})
	}
	return out
}

func (s doSize) accelerators() []provider.Accelerator {
	if s.GPU == nil {
		return nil
	}
	devs := s.GPU.Devices
	if len(devs) == 0 {
		for i := 0; i < s.GPU.Count; i++ {
			devs = append(devs, s.GPU.doGPUDevice)
		}
	}
	out := make([]provider.Accelerator, 0, len(devs))
	for _, d := range devs {
		f := d.Fraction
		if f <= 0 {
			f = 1
		}
		out = append(out, provider.Accelerator{Model: d.Model, MemoryBytes: vramBytes(d), Fraction: f, Unified: d.Unified})
	}
	return out
}

func spotUnqualified(accs []provider.Accelerator) (string, bool) {
	for _, a := range accs {
		m := strings.ToLower(a.Model)
		for _, u := range unqualifiedSpotModels {
			if strings.Contains(m, u) {
				return a.Model, true
			}
		}
	}
	return "", false
}

// offers normalizes the regions/sizes catalog plus account quota into
// provider.Offers. Quota failures leave Quota unknown rather than failing.
func (a *adapter) offers(ctx context.Context, regionsRaw, sizesRaw json.RawMessage) ([]provider.Offer, error) {
	var regions []doRegion
	var sizes []doSize
	if err := json.Unmarshal(regionsRaw, &regions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(sizesRaw, &sizes); err != nil {
		return nil, err
	}
	now := time.Now
	if a.cfg.Now != nil {
		now = a.cfg.Now
	}
	at := now().UTC()

	account, quota := "", provider.Quota{}
	var acct struct {
		Account struct {
			UUID         string `json:"uuid"`
			DropletLimit int    `json:"droplet_limit"`
		} `json:"account"`
	}
	if a.call(ctx, http.MethodGet, "/v2/account", nil, &acct) == nil {
		account = acct.Account.UUID
		var l, gpu struct {
			Meta struct {
				Total *int `json:"total"`
			} `json:"meta"`
		}
		if acct.Account.DropletLimit > 0 &&
			a.call(ctx, http.MethodGet, "/v2/droplets?per_page=1", nil, &l) == nil && l.Meta.Total != nil &&
			a.call(ctx, http.MethodGet, "/v2/droplets?type=gpus&per_page=1", nil, &gpu) == nil && gpu.Meta.Total != nil {
			quota = provider.Quota{Known: true, Limit: acct.Account.DropletLimit, Used: *l.Meta.Total + *gpu.Meta.Total, ObservedAt: at}
		}
	}

	regionOK := map[string]bool{}
	regionSizes := map[string][]string{}
	for _, r := range regions {
		regionOK[r.Slug] = r.Available
		regionSizes[r.Slug] = r.Sizes
	}

	var out []provider.Offer
	for _, z := range sizes {
		accs := z.accelerators()
		rs := slices.Clone(z.Regions)
		for _, r := range regions {
			if slices.Contains(r.Sizes, z.Slug) && !slices.Contains(rs, r.Slug) {
				rs = append(rs, r.Slug)
			}
		}
		slices.Sort(rs)
		for _, region := range rs {
			base := provider.Offer{
				Account: account, Region: region, SKU: z.Slug,
				CPU:           provider.CPU{Arch: z.Arch, Count: z.VCPUs},
				MemoryBytes:   z.Memory << 20,
				Accelerators:  accs,
				Storage:       z.storage(),
				Network:       provider.Network{Known: z.Transfer > 0, TransferBytes: int64(z.Transfer * 1e12)},
				Quota:         quota,
				Qualification: provider.Qualification{Qualified: true},
			}
			av := provider.Availability{State: provider.AvailAvailable, ObservedAt: at}
			if ok, listed := regionOK[region]; listed && !ok {
				av = provider.Availability{State: provider.AvailUnavailable, Reason: "region " + region + " not available", ObservedAt: at}
			} else if !z.Available {
				av = provider.Availability{State: provider.AvailUnavailable, Reason: "size not available", ObservedAt: at}
			}
			base.Availability = av

			od := base
			od.Purchase = provider.PurchaseOnDemand
			od.Lifecycle = provider.Lifecycle{StopKeepsBilling: true, StopKeepsDisk: true, ReleaseDeletes: true,
				Note: "a powered-off droplet still bills; destroy releases it and its boot disk"}
			if z.PriceHourly != nil || z.PriceMonthly != nil {
				od.Price = provider.Price{Known: true, ObservedAt: at}
				if z.PriceHourly != nil {
					od.Price.USDPerHour = *z.PriceHourly
				}
				if z.PriceMonthly != nil {
					od.Price.USDPerMonth = *z.PriceMonthly
				}
			}
			out = append(out, od)

			if z.SpotPriceHourly != nil {
				sp := base
				sp.Purchase = provider.PurchaseSpot
				sp.Price = provider.Price{Known: true, USDPerHour: *z.SpotPriceHourly, ObservedAt: at}
				sp.Lifecycle = provider.Lifecycle{StopKeepsBilling: true, StopKeepsDisk: true, ReleaseDeletes: true,
					Note: "spot prices change daily; instance may be reclaimed"}
				if m, bad := spotUnqualified(accs); bad {
					sp.Qualification = provider.Qualification{Reason: "spot on " + m + " is not qualified for v1"}
				}
				out = append(out, sp)
			}
		}
	}
	return out, nil
}
