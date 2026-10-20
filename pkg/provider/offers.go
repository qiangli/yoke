package provider

import (
	"context"
	"encoding/json"
	"time"
)

// Offer discovery (S451.2) extends OpCapacity: the capacity result's Data
// carries an "offers" array of Offer next to whatever raw catalog the adapter
// already returns. All sizes are bytes, all prices USD; unknown is explicit.

// GiB is one gibibyte in bytes.
const GiB int64 = 1 << 30

// PurchaseMode is how an offer is bought.
type PurchaseMode string

const (
	PurchaseOnDemand PurchaseMode = "on_demand"
	PurchaseSpot     PurchaseMode = "spot"
)

// Accelerator is one accelerator device. MemoryBytes is that device's own
// memory (for a fractional device, its slice). Fraction is the share of a
// physical device (1 = whole). Unified marks memory shared with the host.
type Accelerator struct {
	Vendor      string  `json:"vendor,omitempty"`
	Model       string  `json:"model"`
	MemoryBytes int64   `json:"memory_bytes"`
	Fraction    float64 `json:"fraction,omitempty"`
	Unified     bool    `json:"unified,omitempty"`
}

// CPU is the host CPU shape.
type CPU struct {
	Arch  string `json:"arch,omitempty"`
	Count int    `json:"count"`
}

// Storage is one storage component of an offer.
type Storage struct {
	Kind       string `json:"kind,omitempty"` // "boot", "volume", ...
	SizeBytes  int64  `json:"size_bytes"`
	Persistent bool   `json:"persistent"`
	Attachable bool   `json:"attachable"`
}

// Network holds network characteristics; Known is false when the provider
// does not say.
type Network struct {
	Known         bool  `json:"known"`
	BandwidthMbps int64 `json:"bandwidth_mbps,omitempty"`
	TransferBytes int64 `json:"transfer_bytes,omitempty"` // monthly allowance
}

// Price is a unit price that may be unknown.
type Price struct {
	Known       bool      `json:"known"`
	USDPerHour  float64   `json:"usd_per_hour,omitempty"`
	USDPerMonth float64   `json:"usd_per_month,omitempty"`
	ObservedAt  time.Time `json:"observed_at,omitempty"`
}

// AvailState is whether an offer can currently be bought.
type AvailState string

const (
	AvailAvailable   AvailState = "available"
	AvailUnavailable AvailState = "unavailable"
	AvailUnknown     AvailState = "unknown"
)

// Availability is the (advertised) availability in the offer's region.
type Availability struct {
	State      AvailState `json:"state"`
	Reason     string     `json:"reason,omitempty"`
	ObservedAt time.Time  `json:"observed_at,omitempty"`
}

// Quota is account-level headroom for the offer's resource class; it is
// distinct from the advertised catalog. Known is false when not observed.
type Quota struct {
	Known      bool      `json:"known"`
	Limit      int       `json:"limit,omitempty"`
	Used       int       `json:"used,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// Exhausted reports a known quota with no headroom.
func (q Quota) Exhausted() bool { return q.Known && q.Used >= q.Limit }

// Allocation is what is actually allocated/observed on a host, distinct from
// what the catalog advertises. Nil means nothing observed.
type Allocation struct {
	Accelerators []Accelerator `json:"accelerators,omitempty"`
	ObservedAt   time.Time     `json:"observed_at,omitempty"`
}

// Lifecycle states what stop and release mean for cost and data.
type Lifecycle struct {
	StopKeepsBilling bool   `json:"stop_keeps_billing"`
	StopKeepsDisk    bool   `json:"stop_keeps_disk"`
	ReleaseDeletes   bool   `json:"release_deletes"`
	Note             string `json:"note,omitempty"`
}

// Qualification says whether the offer is usable under v1 policy.
type Qualification struct {
	Qualified bool   `json:"qualified"`
	Reason    string `json:"reason,omitempty"`
}

// Offer is one normalized purchasable shape: advertised catalog data plus
// optional account quota and observed allocation.
type Offer struct {
	Account       string        `json:"account,omitempty"`
	Region        string        `json:"region"`
	SKU           string        `json:"sku"`
	Purchase      PurchaseMode  `json:"purchase"`
	CPU           CPU           `json:"cpu"`
	MemoryBytes   int64         `json:"memory_bytes"`
	Accelerators  []Accelerator `json:"accelerators,omitempty"`
	Storage       []Storage     `json:"storage,omitempty"`
	Network       Network       `json:"network"`
	Price         Price         `json:"price"`
	Availability  Availability  `json:"availability"`
	Quota         Quota         `json:"quota"`
	Allocation    *Allocation   `json:"allocation,omitempty"`
	Lifecycle     Lifecycle     `json:"lifecycle"`
	Qualification Qualification `json:"qualification"`
}

// PerDeviceMemoryBytes returns each accelerator's own memory, in order.
func (o Offer) PerDeviceMemoryBytes() []int64 {
	out := make([]int64, len(o.Accelerators))
	for i, a := range o.Accelerators {
		out[i] = a.MemoryBytes
	}
	return out
}

// SingleDeviceFitBytes is the largest model/working set one device can hold:
// the per-device maximum, never a sum across devices.
func (o Offer) SingleDeviceFitBytes() int64 {
	var max int64
	for _, a := range o.Accelerators {
		if a.MemoryBytes > max {
			max = a.MemoryBytes
		}
	}
	return max
}

// FitsSingleDevice reports whether need bytes fit on one device.
func (o Offer) FitsSingleDevice(need int64) bool {
	return need <= o.SingleDeviceFitBytes()
}

// Placeable reports whether the offer can be bought now under v1 policy and
// why not. Unknown availability or quota does not block; it is surfaced by
// the caller from the Unknown states.
func (o Offer) Placeable() (bool, string) {
	switch {
	case !o.Qualification.Qualified:
		return false, "not qualified for v1: " + o.Qualification.Reason
	case o.Availability.State == AvailUnavailable:
		return false, "unavailable in region: " + o.Availability.Reason
	case o.Quota.Exhausted():
		return false, "account quota exhausted"
	}
	return true, ""
}

// OffersData is the "offers" payload of an OpCapacity result's Data.
type OffersData struct {
	Offers []Offer `json:"offers"`
}

// DiscoverOffers runs OpCapacity on a and returns its normalized offers. An
// adapter that does not emit offers yields none.
func DiscoverOffers(ctx context.Context, a Adapter) ([]Offer, error) {
	res, err := a.Do(ctx, Request{ProtocolVersion: ProtocolVersion, Op: OpCapacity})
	if err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, res.Error
	}
	var d OffersData
	if len(res.Data) > 0 {
		if err := json.Unmarshal(res.Data, &d); err != nil {
			return nil, err
		}
	}
	return d.Offers, nil
}
