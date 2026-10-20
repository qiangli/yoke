package provider

import (
	"fmt"
	"time"
)

// Model-fit planning (S451.3). Fit is judged per device, never on a sum of
// device memory; the S451.5 readiness load remains the decisive runtime check.

// ModelSpec carries the registry fields (Digest, Quantization, ContextLength)
// plus the weight/KV metadata the estimate needs. Zero means unknown.
type ModelSpec struct {
	Digest          string `json:"digest,omitempty"`
	Quantization    string `json:"quantization,omitempty"`
	ContextLength   int    `json:"context_length,omitempty"`
	Params          int64  `json:"params,omitempty"`
	WeightBytes     int64  `json:"weight_bytes,omitempty"`
	KVBytesPerToken int64  `json:"kv_bytes_per_token,omitempty"`
}

type LifecycleMode string

const (
	LifecyclePersistent LifecycleMode = "persistent"
	LifecycleEphemeral  LifecycleMode = "ephemeral"
)

// Workload is what the worker must run.
type Workload struct {
	Model          ModelSpec
	Context        int
	Concurrency    int
	AllowCPU       bool
	AllowSpot      bool
	Lifecycle      LifecycleMode
	MaxLifetime    time.Duration // ignored for persistent
	CleanupTrigger string
	Hours          float64 // expected run time for the quote
}

type Placement string

const (
	PlacementGPU         Placement = "gpu"
	PlacementUnified     Placement = "unified_memory"
	PlacementCPUFallback Placement = "cpu_fallback"
)

// Charge is one projected cost line; Known=false is an explicit unknown.
type Charge struct {
	Known      bool      `json:"known"`
	USD        float64   `json:"usd,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// CostPlan separates quoted (projected) from measured (nil until observed).
type CostPlan struct {
	Compute  Charge   `json:"compute"`
	Storage  Charge   `json:"storage"`
	Network  Charge   `json:"network"`
	Measured *float64 `json:"measured_usd,omitempty"`
}

// Plan explains one offer's fit and cost.
type Plan struct {
	Offer          Offer         `json:"offer"`
	Fits           bool          `json:"fits"`
	Reason         string        `json:"reason,omitempty"`
	Placement      Placement     `json:"placement,omitempty"`
	NeedBytes      int64         `json:"need_bytes,omitempty"`
	Uncertain      []string      `json:"uncertain,omitempty"`
	Cost           CostPlan      `json:"cost"`
	LifecycleMode  LifecycleMode `json:"lifecycle_mode"`
	Purchase       PurchaseMode  `json:"purchase"`
	MaxLifetime    time.Duration `json:"max_lifetime,omitempty"`
	CleanupTrigger string        `json:"cleanup_trigger,omitempty"`
	Notes          []string      `json:"notes,omitempty"`
}

const (
	defaultKVBytesPerToken = 128 << 10
	runtimeBufferBytes     = 1 * GiB
)

// PlanFit judges whether w fits o and projects cost and lifecycle.
func PlanFit(o Offer, w Workload) Plan {
	p := Plan{Offer: o, Purchase: o.Purchase, LifecycleMode: w.Lifecycle, CleanupTrigger: w.CleanupTrigger}
	if w.Lifecycle != LifecyclePersistent {
		p.LifecycleMode = LifecycleEphemeral
		p.MaxLifetime = w.MaxLifetime
	}
	reject := func(format string, a ...any) Plan {
		p.Reason = fmt.Sprintf(format, a...)
		return p
	}
	if ok, why := o.Placeable(); !ok {
		return reject("%s", why)
	}
	if o.Purchase == PurchaseSpot && !w.AllowSpot {
		return reject("spot requires explicit opt-in; no silent fallback from on-demand")
	}
	if w.Model.WeightBytes <= 0 {
		return reject("weight size unknown; cannot estimate fit")
	}

	kv := w.Model.KVBytesPerToken
	if kv <= 0 {
		kv = defaultKVBytesPerToken
		p.Uncertain = append(p.Uncertain, "KV bytes per token unknown; assumed 128 KiB")
	}
	ctx := w.Context
	if ctx <= 0 {
		ctx = w.Model.ContextLength
	}
	if ctx <= 0 {
		ctx = 4096
		p.Uncertain = append(p.Uncertain, "context unknown; assumed 4096")
	}
	conc := max(w.Concurrency, 1)
	raw := w.Model.WeightBytes + kv*int64(ctx)*int64(conc) + runtimeBufferBytes
	p.NeedBytes = raw + raw/10 // 10% headroom

	switch {
	case len(o.Accelerators) > 0:
		best := o.SingleDeviceFitBytes()
		if !o.FitsSingleDevice(p.NeedBytes) {
			return reject("needs %d GiB, exceeds per-device memory (largest device %d GiB of %d devices; memory is not contiguous across devices)",
				p.NeedBytes/GiB, best/GiB, len(o.Accelerators))
		}
		p.Placement = PlacementGPU
		for _, a := range o.Accelerators {
			if a.Unified && a.MemoryBytes >= p.NeedBytes {
				p.Placement = PlacementUnified
			}
		}
	case w.AllowCPU:
		if p.NeedBytes > o.MemoryBytes {
			return reject("needs %d GiB, exceeds host memory %d GiB for CPU fallback", p.NeedBytes/GiB, o.MemoryBytes/GiB)
		}
		p.Placement = PlacementCPUFallback
		p.Notes = append(p.Notes, "explicit CPU fallback: slow, uses host RAM")
	default:
		return reject("no accelerator and CPU fallback not requested")
	}
	p.Fits = true
	p.cost(o, w)
	return p
}

func (p *Plan) cost(o Offer, w Workload) {
	if o.Price.Known && o.Price.USDPerHour > 0 && w.Hours > 0 {
		p.Cost.Compute = Charge{Known: true, USD: o.Price.USDPerHour * w.Hours, ObservedAt: o.Price.ObservedAt}
	}
	if o.Purchase == PurchaseSpot {
		p.Notes = append(p.Notes, "spot price observed at "+o.Price.ObservedAt.UTC().Format(time.RFC3339)+"; provider interruption notice may exceed the TTL")
	}
	if p.LifecycleMode == LifecycleEphemeral {
		p.Notes = append(p.Notes, "TTL is not a guaranteed billing cap")
	} else {
		p.MaxLifetime = 0
		p.Notes = append(p.Notes, "persistent worker has no expiry")
	}
	if o.Lifecycle.StopKeepsDisk || o.Lifecycle.StopKeepsBilling {
		p.Notes = append(p.Notes, "retained storage and billing after stop until released")
	}
	p.Notes = append(p.Notes, "provider interruption can end the worker early")
}
