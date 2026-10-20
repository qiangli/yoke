package provider

import (
	"strings"
	"testing"
	"time"
)

func gpuOffer(n int, gib int64) Offer {
	o := Offer{SKU: "gpu", Purchase: PurchaseOnDemand, MemoryBytes: 128 * GiB,
		Qualification: Qualification{Qualified: true},
		Price:         Price{Known: true, USDPerHour: 2, ObservedAt: time.Unix(1000, 0)}}
	for i := 0; i < n; i++ {
		o.Accelerators = append(o.Accelerators, Accelerator{Model: "x", MemoryBytes: gib * GiB})
	}
	return o
}

func baseWork() Workload {
	return Workload{Model: ModelSpec{WeightBytes: 40 * GiB, KVBytesPerToken: 128 << 10},
		Context: 8192, Concurrency: 1, Lifecycle: LifecycleEphemeral, Hours: 1}
}

func TestPlanFit_TwoDevicesNotContiguous(t *testing.T) {
	p := PlanFit(gpuOffer(2, 24), baseWork())
	if p.Fits {
		t.Fatalf("40 GiB on 2x24 GiB must be rejected: %+v", p)
	}
	if !strings.Contains(p.Reason, "exceeds per-device memory") {
		t.Fatalf("reason = %q", p.Reason)
	}
}

func TestPlanFit_Table(t *testing.T) {
	small := func(w *Workload) { w.Model.WeightBytes = 10 * GiB }
	cases := []struct {
		name      string
		offer     Offer
		mut       func(*Workload)
		fits      bool
		placement Placement
		reason    string
	}{
		{"fits 24", gpuOffer(1, 24), small, true, PlacementGPU, ""},
		{"context flips", gpuOffer(1, 24), func(w *Workload) { small(w); w.Context = 1 << 20 }, false, "", "exceeds per-device memory"},
		{"concurrency flips", gpuOffer(1, 24), func(w *Workload) { small(w); w.Concurrency = 512 }, false, "", "exceeds per-device memory"},
		{"cpu fallback explicit", gpuOffer(0, 0), func(w *Workload) { small(w); w.AllowCPU = true }, true, PlacementCPUFallback, ""},
		{"cpu not opted in", gpuOffer(0, 0), small, false, "", "no accelerator"},
		{"cpu ram too small", func() Offer { o := gpuOffer(0, 0); o.MemoryBytes = 8 * GiB; return o }(),
			func(w *Workload) { small(w); w.AllowCPU = true }, false, "", "exceeds host memory"},
		{"unified", func() Offer {
			o := gpuOffer(1, 64)
			o.Accelerators[0].Unified = true
			return o
		}(), func(w *Workload) { w.Model.WeightBytes = 40 * GiB }, true, PlacementUnified, ""},
		{"spot without opt-in", func() Offer { o := gpuOffer(1, 24); o.Purchase = PurchaseSpot; return o }(), small, false, "", "spot requires explicit opt-in"},
		{"spot opt-in", func() Offer { o := gpuOffer(1, 24); o.Purchase = PurchaseSpot; return o }(),
			func(w *Workload) { small(w); w.AllowSpot = true }, true, PlacementGPU, ""},
		{"unplaceable", func() Offer { o := gpuOffer(1, 24); o.Quota = Quota{Known: true, Limit: 1, Used: 1}; return o }(), small, false, "", "quota"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := baseWork()
			c.mut(&w)
			p := PlanFit(c.offer, w)
			if p.Fits != c.fits {
				t.Fatalf("fits=%v reason=%q", p.Fits, p.Reason)
			}
			if c.fits && p.Placement != c.placement {
				t.Fatalf("placement=%q", p.Placement)
			}
			if !c.fits && !strings.Contains(p.Reason, c.reason) {
				t.Fatalf("reason=%q want %q", p.Reason, c.reason)
			}
		})
	}
}

func TestPlanFit_UnknownMetadataAndCost(t *testing.T) {
	o := gpuOffer(1, 24)
	o.Price = Price{}
	w := baseWork()
	w.Model = ModelSpec{WeightBytes: 10 * GiB}
	p := PlanFit(o, w)
	if !p.Fits || len(p.Uncertain) == 0 {
		t.Fatalf("want fit with uncertainty: %+v", p)
	}
	if p.Cost.Compute.Known || p.Cost.Storage.Known || p.Cost.Network.Known {
		t.Fatalf("unknown costs must be labeled: %+v", p.Cost)
	}
	if p.Cost.Measured != nil {
		t.Fatal("quoted and measured must be distinct; measured absent")
	}
	w.Model.WeightBytes = 0
	if p := PlanFit(o, w); p.Fits || !strings.Contains(p.Reason, "weight size unknown") {
		t.Fatalf("unknown weights: %+v", p)
	}
}

func TestPlanFit_QuotedCostAndLifecycle(t *testing.T) {
	w := baseWork()
	w.Model.WeightBytes = 10 * GiB
	w.Hours = 3
	w.MaxLifetime = time.Hour
	w.CleanupTrigger = "work-complete"
	o := gpuOffer(1, 24)
	o.Purchase = PurchaseSpot
	w.AllowSpot = true
	o.Lifecycle = Lifecycle{StopKeepsDisk: true, StopKeepsBilling: true}
	p := PlanFit(o, w)
	if !p.Cost.Compute.Known || p.Cost.Compute.USD != 6 || p.Cost.Compute.ObservedAt.IsZero() {
		t.Fatalf("compute: %+v", p.Cost.Compute)
	}
	if p.LifecycleMode != LifecycleEphemeral || p.MaxLifetime != time.Hour || p.CleanupTrigger != "work-complete" {
		t.Fatalf("lifecycle: %+v", p)
	}
	joined := strings.Join(p.Notes, "|")
	for _, s := range []string{"not a guaranteed billing cap", "interruption", "retained storage"} {
		if !strings.Contains(joined, s) {
			t.Errorf("notes missing %q: %v", s, p.Notes)
		}
	}
	w.Lifecycle = LifecyclePersistent
	w.MaxLifetime = time.Hour
	if p := PlanFit(gpuOffer(1, 24), w); p.MaxLifetime != 0 {
		t.Fatalf("persistent has no expiry: %v", p.MaxLifetime)
	}
}
