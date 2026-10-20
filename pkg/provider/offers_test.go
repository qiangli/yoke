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
