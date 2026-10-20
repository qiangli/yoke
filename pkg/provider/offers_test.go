package provider

import "testing"

func TestDiscoverOffers_MultiGPUNotSummedForSingleDeviceFit(t *testing.T) {
	o := Offer{Accelerators: []Accelerator{
		{Model: "L4", MemoryBytes: 24 * GiB, Fraction: 1},
		{Model: "L4", MemoryBytes: 24 * GiB, Fraction: 1},
	}}
	per := o.PerDeviceMemoryBytes()
	if len(per) != 2 || per[0] != 24*GiB || per[1] != 24*GiB {
		t.Fatalf("per-device memory = %v, want [24GiB 24GiB]", per)
	}
	if got := o.SingleDeviceFitBytes(); got != 24*GiB {
		t.Fatalf("single-device fit = %d GiB, want 24 GiB (never 48)", got/GiB)
	}
}

func TestDiscoverOffers_FitAndPlaceable(t *testing.T) {
	two := Offer{Qualification: Qualification{Qualified: true}, Accelerators: []Accelerator{
		{Model: "L4", MemoryBytes: 24 * GiB}, {Model: "L4", MemoryBytes: 24 * GiB}}}
	if two.FitsSingleDevice(40 * GiB) {
		t.Fatal("two 24 GiB devices must not fit 40 GiB")
	}
	het := Offer{Accelerators: []Accelerator{{Model: "A", MemoryBytes: 16 * GiB}, {Model: "B", MemoryBytes: 80 * GiB, Fraction: 0.5}}}
	if het.SingleDeviceFitBytes() != 80*GiB {
		t.Fatalf("heterogeneous fit = %d", het.SingleDeviceFitBytes())
	}
	if (Offer{}).SingleDeviceFitBytes() != 0 {
		t.Fatal("no GPU must fit nothing")
	}
	if ok, _ := two.Placeable(); !ok {
		t.Fatal("qualified, unknown availability must be placeable")
	}
	ex := two
	ex.Quota = Quota{Known: true, Limit: 2, Used: 2}
	if ok, _ := ex.Placeable(); ok {
		t.Fatal("exhausted quota must not be placeable")
	}
	un := two
	un.Availability = Availability{State: AvailUnavailable}
	if ok, _ := un.Placeable(); ok {
		t.Fatal("unavailable region must not be placeable")
	}
	nq := two
	nq.Qualification = Qualification{Reason: "x"}
	if ok, _ := nq.Placeable(); ok {
		t.Fatal("unqualified must not be placeable")
	}
}

func TestDiscoverOffers_ObservedDeviceMismatch(t *testing.T) {
	adv := []Accelerator{{Vendor: "nvidia", Model: "NVIDIA L40S", MemoryBytes: 48 * GiB}, {Model: "L40S", MemoryBytes: 48 * GiB}}

	ok := CompareAccelerators(adv, []Accelerator{{Model: "l40s", MemoryBytes: 48 * GiB}, {Model: "NVIDIA-L40S", MemoryBytes: 47 * GiB}}, 0.05)
	if !ok.Match() {
		t.Fatalf("within tolerance must match: %+v", ok)
	}

	cnt := CompareAccelerators(adv, []Accelerator{{Model: "L40S", MemoryBytes: 48 * GiB}}, 0)
	if cnt.Match() || cnt.Advertised != 2 || cnt.Observed != 1 || cnt.Mismatches[0].Kind != MismatchCount {
		t.Fatalf("count mismatch: %+v", cnt)
	}

	mem := CompareAccelerators(adv, []Accelerator{{Model: "L40S", MemoryBytes: 48 * GiB}, {Model: "L40S", MemoryBytes: 24 * GiB}}, 0.05)
	if mem.Match() || len(mem.Mismatches) != 1 || mem.Mismatches[0].Kind != MismatchMemory || mem.Mismatches[0].ObservedBytes != 24*GiB {
		t.Fatalf("memory mismatch: %+v", mem)
	}

	mod := CompareAccelerators(adv, []Accelerator{{Model: "L40S", MemoryBytes: 48 * GiB}, {Model: "T4", MemoryBytes: 48 * GiB}}, 0)
	if mod.Match() || mod.Mismatches[0].Kind != MismatchModel {
		t.Fatalf("model mismatch: %+v", mod)
	}

	unk := CompareAccelerators(adv, []Accelerator{{Model: "L40S"}, {Model: "L40S"}}, 0)
	if !unk.Match() {
		t.Fatalf("unobserved memory must not mismatch: %+v", unk)
	}

	o := Offer{Accelerators: adv}
	if c := o.CompareObserved(); c.Match() || c.Observed != 0 {
		t.Fatalf("no allocation observed must mismatch on count: %+v", c)
	}
	o.Allocation = &Allocation{Accelerators: adv}
	if !o.CompareObserved().Match() {
		t.Fatal("identical allocation must match")
	}
}
