package digitalocean_test

import (
	"context"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

var observed = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func vram(n float64) map[string]any { return map[string]any{"amount": n, "unit": "gib"} }

func discover(t *testing.T, fake *fakedo.Server) map[string]provider.Offer {
	t.Helper()
	a, err := digitalocean.New(digitalocean.Config{BaseURL: fake.URL, Token: fakedo.TestToken, Owner: "t1", Now: func() time.Time { return observed }})
	if err != nil {
		t.Fatal(err)
	}
	offers, err := provider.DiscoverOffers(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]provider.Offer{}
	for _, o := range offers {
		m[o.SKU+"@"+o.Region+"/"+string(o.Purchase)] = o
	}
	return m
}

func catalog(fake *fakedo.Server) {
	fake.SetCatalog(
		[]map[string]any{
			{"slug": "nyc", "available": true, "sizes": []string{}},
			{"slug": "gone", "available": false, "sizes": []string{"gpu-1x"}},
		},
		[]map[string]any{
			{"slug": "cpu-2", "vcpus": 2, "memory": 4096, "disk": 80, "transfer": 4.0, "price_hourly": 0.03, "price_monthly": 24.0, "available": true, "regions": []string{"nyc"}},
			{"slug": "cpu-noprice", "vcpus": 4, "memory": 8192, "disk": 160, "available": true, "regions": []string{"nyc"}},
			{"slug": "gpu-1x", "vcpus": 8, "memory": 65536, "disk": 400, "price_hourly": 1.5, "available": true, "regions": []string{"nyc"},
				"gpu_info": map[string]any{"count": 1, "model": "nvidia_l40s", "vram": vram(48)}},
			{"slug": "gpu-2x", "vcpus": 16, "memory": 131072, "disk": 800, "price_hourly": 3.0, "available": true, "regions": []string{"nyc"},
				"gpu_info": map[string]any{"count": 2, "model": "nvidia_l4", "vram": vram(24)}},
			{"slug": "gpu-mixed", "vcpus": 16, "memory": 131072, "disk": 800, "price_hourly": 5.0, "available": true, "regions": []string{"nyc"},
				"gpu_info": map[string]any{"devices": []map[string]any{
					{"model": "nvidia_a100", "vram": vram(80)},
					{"model": "nvidia_a100", "fraction": 0.25, "vram": vram(20)},
					{"model": "amd_mi300x", "vram": vram(192)},
				}}},
			{"slug": "gpu-b300", "vcpus": 32, "memory": 262144, "disk": 1000, "price_hourly": 20.0, "spot_price_hourly": 9.0, "available": true, "regions": []string{"nyc"},
				"gpu_info": map[string]any{"count": 8, "model": "nvidia_b300", "vram": vram(288)}},
			{"slug": "gpu-spot-ok", "vcpus": 8, "memory": 65536, "disk": 400, "price_hourly": 1.5, "spot_price_hourly": 0.6, "available": true, "regions": []string{"nyc"},
				"gpu_info": map[string]any{"count": 1, "model": "nvidia_l40s", "vram": vram(48)}},
		})
}

func TestDiscoverOffers_DigitalOcean(t *testing.T) {
	fake := fakedo.New(t)
	catalog(fake)
	fake.SetAccount(map[string]any{"uuid": "acct-9", "droplet_limit": 3})
	fake.AddDroplet("a")
	got := discover(t, fake)

	cpu := got["cpu-2@nyc/on_demand"]
	if cpu.Account != "acct-9" || cpu.CPU.Count != 2 || cpu.MemoryBytes != 4*provider.GiB {
		t.Fatalf("cpu offer = %+v", cpu)
	}
	if len(cpu.Accelerators) != 0 || cpu.FitsSingleDevice(1) {
		t.Fatal("cpu-only/missing GPU must fit nothing on a device")
	}
	if !cpu.Price.Known || cpu.Price.ObservedAt != observed || cpu.Availability.ObservedAt != observed {
		t.Fatalf("price/availability stamps: %+v %+v", cpu.Price, cpu.Availability)
	}
	if s := cpu.Storage[0]; !s.Persistent || s.Attachable || s.SizeBytes != 80*provider.GiB {
		t.Fatalf("storage = %+v", s)
	}
	if !cpu.Network.Known || cpu.Network.TransferBytes != 4e12 {
		t.Fatalf("network = %+v", cpu.Network)
	}
	if !cpu.Lifecycle.StopKeepsBilling || !cpu.Lifecycle.ReleaseDeletes {
		t.Fatalf("lifecycle = %+v", cpu.Lifecycle)
	}
	if !cpu.Quota.Known || cpu.Quota.Limit != 3 || cpu.Quota.Used != 1 || cpu.Quota.Exhausted() {
		t.Fatalf("quota = %+v", cpu.Quota)
	}

	if np := got["cpu-noprice@nyc/on_demand"]; np.Price.Known || np.Price.USDPerHour != 0 {
		t.Fatalf("unknown price reported known: %+v", np.Price)
	}

	one := got["gpu-1x@nyc/on_demand"]
	if one.SingleDeviceFitBytes() != 48*provider.GiB || len(one.Accelerators) != 1 {
		t.Fatalf("one gpu = %+v", one.Accelerators)
	}

	two := got["gpu-2x@nyc/on_demand"]
	if len(two.Accelerators) != 2 || two.SingleDeviceFitBytes() != 24*provider.GiB || two.FitsSingleDevice(40*provider.GiB) {
		t.Fatalf("2x24 GiB: fit=%d", two.SingleDeviceFitBytes()/provider.GiB)
	}

	mixed := got["gpu-mixed@nyc/on_demand"]
	if len(mixed.Accelerators) != 3 || mixed.Accelerators[1].Fraction != 0.25 || mixed.Accelerators[0].Fraction != 1 {
		t.Fatalf("mixed = %+v", mixed.Accelerators)
	}
	if mixed.SingleDeviceFitBytes() != 192*provider.GiB {
		t.Fatalf("mixed fit = %d GiB", mixed.SingleDeviceFitBytes()/provider.GiB)
	}

	gone := got["gpu-1x@gone/on_demand"]
	if gone.Availability.State != provider.AvailUnavailable {
		t.Fatalf("regional unavailability = %+v", gone.Availability)
	}
	if ok, _ := gone.Placeable(); ok {
		t.Fatal("unavailable region placeable")
	}

	b300 := got["gpu-b300@nyc/spot"]
	if b300.Qualification.Qualified || b300.Price.USDPerHour != 9.0 {
		t.Fatalf("b300 spot = %+v", b300)
	}
	if ok, why := b300.Placeable(); ok || why == "" {
		t.Fatal("unqualified spot GPU placeable")
	}
	if !got["gpu-b300@nyc/on_demand"].Qualification.Qualified {
		t.Fatal("on-demand B300 is not a spot qualification issue")
	}
	if !got["gpu-spot-ok@nyc/spot"].Qualification.Qualified {
		t.Fatal("other spot SKUs must not be blocked by a central list")
	}
}

func TestDiscoverOffers_DigitalOceanExhaustedQuota(t *testing.T) {
	fake := fakedo.New(t)
	catalog(fake)
	fake.SetAccount(map[string]any{"uuid": "acct-9", "droplet_limit": 1})
	fake.AddDroplet("a")
	o := discover(t, fake)["gpu-1x@nyc/on_demand"]
	if !o.Quota.Exhausted() {
		t.Fatalf("quota = %+v", o.Quota)
	}
	if ok, _ := o.Placeable(); ok {
		t.Fatal("exhausted quota placeable")
	}
}

func TestDiscoverOffers_DigitalOceanQuotaIncludesGPUDroplets(t *testing.T) {
	fake := fakedo.New(t)
	catalog(fake)
	fake.SetAccount(map[string]any{"uuid": "acct-9", "droplet_limit": 2})
	fake.AddDroplet("cpu")
	fake.AddGPUDroplet("gpu")
	for _, o := range discover(t, fake) {
		if !o.Quota.Known || o.Quota.Used != 2 || !o.Quota.Exhausted() {
			t.Fatalf("quota omitted GPU droplet: %+v", o.Quota)
		}
	}
}

func TestDiscoverOffers_DigitalOceanDefaultCatalogStillServed(t *testing.T) {
	fake := fakedo.New(t)
	got := discover(t, fake)
	if o := got["s-2vcpu-4gb@r1/on_demand"]; o.SKU == "" || o.Quota.Limit != 25 {
		t.Fatalf("default catalog offer = %+v", o)
	}
}

func TestDiscoverOffers_DigitalOceanDiskInfoAndUnknownVRAMUnit(t *testing.T) {
	fake := fakedo.New(t)
	fake.SetCatalog(
		[]map[string]any{{"slug": "nyc", "available": true, "sizes": []string{}}},
		[]map[string]any{
			{"slug": "gpu-disk", "vcpus": 8, "memory": 65536, "disk": 999, "available": true, "regions": []string{"nyc"},
				"disk_info": []map[string]any{
					{"type": "boot", "size": map[string]any{"amount": 720, "unit": "gib"}},
					{"type": "scratch", "size": map[string]any{"amount": 5, "unit": "tib"}},
					{"type": "mystery", "size": map[string]any{"amount": 1, "unit": "furlong"}},
				},
				"gpu_info": map[string]any{"count": 1, "model": "nvidia_x", "vram": map[string]any{"amount": 80, "unit": "parsecs"}}},
			{"slug": "gpu-mb", "vcpus": 8, "memory": 65536, "disk": 100, "available": true, "regions": []string{"nyc"},
				"gpu_info": map[string]any{"count": 1, "model": "nvidia_y", "vram": map[string]any{"amount": 24000, "unit": "mb"}}},
		})
	got := discover(t, fake)

	o := got["gpu-disk@nyc/on_demand"]
	if len(o.Storage) != 2 {
		t.Fatalf("storage = %+v (unknown-unit disk must be dropped, legacy disk ignored)", o.Storage)
	}
	boot, scratch := o.Storage[0], o.Storage[1]
	if boot.Kind != "boot" || !boot.Persistent || boot.SizeBytes != 720*provider.GiB {
		t.Fatalf("boot = %+v", boot)
	}
	if scratch.Kind != "scratch" || scratch.Persistent || scratch.Attachable || scratch.SizeBytes != 5<<40 {
		t.Fatalf("scratch = %+v", scratch)
	}
	if len(o.Accelerators) != 1 || o.Accelerators[0].MemoryBytes != 0 || o.FitsSingleDevice(1) {
		t.Fatalf("unknown VRAM unit must not be guessed: %+v", o.Accelerators)
	}
	if mb := got["gpu-mb@nyc/on_demand"].Accelerators[0].MemoryBytes; mb != 24000*1_000_000 {
		t.Fatalf("decimal MB = %d, want exactly 24e9", mb)
	}
}
