package ladder

import (
	"github.com/qiangli/yoke/pkg/ladder/blame"
	"time"
)

// EventSchema identifies the append-only ledger format.
const EventSchema = "bashy-ladder-events-v1"

type EventKind string

const (
	EventKindDelivery   EventKind = "delivery"
	EventKindRegression EventKind = "regression"
	EventKindEstimate   EventKind = "estimate"
	EventKindManage     EventKind = "manage"
	EventKindCert       EventKind = "cert"
	EventKindSeat       EventKind = "seat"
	EventKindCorrection EventKind = "correction"
	// EventKindSeed sets an agent's starting rating for one duty from
	// public data (SeedR/SeedRD). It is a prior, never a rated event.
	EventKindSeed EventKind = "seed"
)

// Event is one immutable item in the rating ledger. ID is the stable correction reference.
type Event struct {
	Schema   string            `json:"schema,omitempty"`
	ID       string            `json:"id,omitempty"`
	At       time.Time         `json:"at"`
	Season   int               `json:"season"`
	Kind     EventKind         `json:"kind"`
	Agent    string            `json:"agent"`
	Duty     Duty              `json:"duty,omitempty"`
	Story    string            `json:"story,omitempty"`
	Points   Points            `json:"points,omitempty"`
	Outcome  float64           `json:"outcome"`
	Blame    blame.Attribution `json:"blame,omitempty"`
	Estimate Points            `json:"estimate,omitempty"`
	Reviewer string            `json:"reviewer,omitempty"`
	// Author is who wrote the story (usually the manager who wrote or split
	// it); a spec-class failure charges the author.
	Author   string `json:"author,omitempty"`
	CapsUsed struct {
		Turns       int `json:"turns"`
		WallSeconds int `json:"wall_seconds"`
	} `json:"caps_used,omitempty"`
	Cost        float64     `json:"cost,omitempty"`
	Sprint      int         `json:"sprint,omitempty"`
	Score       float64     `json:"score,omitempty"`
	Opponent    Rating      `json:"opponent,omitempty"`
	Supersedes  string      `json:"supersedes,omitempty"`
	Note        string      `json:"note,omitempty"`
	Cert        Certificate `json:"cert,omitempty"`
	Provisional int         `json:"provisional,omitempty"`
	// SeedR and SeedRD are a seed event's starting rating and deviation.
	SeedR  float64 `json:"seed_r,omitempty"`
	SeedRD float64 `json:"seed_rd,omitempty"`
}

func eventKnownKind(k EventKind) bool {
	switch k {
	case EventKindDelivery, EventKindRegression, EventKindEstimate, EventKindManage, EventKindCert, EventKindSeat, EventKindCorrection, EventKindSeed:
		return true
	}
	return false
}
