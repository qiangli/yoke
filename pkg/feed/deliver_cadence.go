package feed

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DeliveryPrefs are per-channel delivery-cadence preferences, stored
// locally by the reader's bashy. The zero value is equivalent to
// DefaultDeliveryPrefs.
type DeliveryPrefs struct {
	Interval   time.Duration // default 4h
	MaxItems   int           // default 10
	QuietStart int           // minutes after local midnight, default 22*60
	QuietEnd   int           // minutes after local midnight, default 7*60 (the window may wrap midnight)
	Immediate  []string      // author ids whose posts break quiet hours and the interval
	Off        bool
}

// DefaultDeliveryPrefs returns the standard cadence: at most one batch
// every 4 hours, up to 10 items, quiet from 22:00 to 07:00 local time.
// Set QuietStart == QuietEnd (nonzero) to disable quiet hours, or Off to
// stop delivery entirely.
func DefaultDeliveryPrefs() DeliveryPrefs {
	return DeliveryPrefs{
		Interval:   4 * time.Hour,
		MaxItems:   10,
		QuietStart: 22 * 60,
		QuietEnd:   7 * 60,
	}
}

// Candidate is one unread post under consideration for delivery.
type Candidate struct {
	ID, Author, Headline, Link string
	Created                    time.Time
}

// Batch is one delivery message: the items to show, plus a collapsed
// count of the rest with a link to the web inbox.
type Batch struct {
	Items    []Candidate
	More     int    // count collapsed beyond MaxItems
	MoreLink string // web inbox URL, set when More > 0
}

// PlanDelivery decides whether to send now and what. now is in the
// reader's local zone. last is when this channel last sent (zero =
// never). read reports items read anywhere; those are dropped first and
// never counted, so a read item cannot inflate More. The returned batch
// is false when nothing should be sent now. PlanDelivery is pure: it
// never performs I/O and never mutates pending.
func PlanDelivery(now, last time.Time, pending []Candidate, read func(id string) bool, p DeliveryPrefs, inboxURL string) (Batch, bool) {
	if p.Off {
		return Batch{}, false
	}
	p = normalizePrefs(p)

	var unread []Candidate
	for _, c := range pending {
		if read != nil && read(c.ID) {
			continue
		}
		unread = append(unread, c)
	}
	if len(unread) == 0 {
		return Batch{}, false
	}

	immediate := make(map[string]bool, len(p.Immediate))
	for _, id := range p.Immediate {
		immediate[id] = true
	}

	// Immediate authors break both quiet hours and the interval; every
	// other item waits for a quiet-free clock and an elapsed interval.
	var eligible []Candidate
	if !inQuietHours(now, p) && (last.IsZero() || now.Sub(last) >= p.Interval) {
		eligible = unread
	} else {
		for _, c := range unread {
			if immediate[c.Author] {
				eligible = append(eligible, c)
			}
		}
	}
	if len(eligible) == 0 {
		return Batch{}, false
	}

	// Immediate authors first, then newest Created first; stable for ties.
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := immediate[eligible[i].Author], immediate[eligible[j].Author]
		if a != b {
			return a
		}
		return eligible[i].Created.After(eligible[j].Created)
	})

	batch := Batch{Items: eligible}
	if len(eligible) > p.MaxItems {
		batch.Items = eligible[:p.MaxItems]
		batch.More = len(eligible) - p.MaxItems
		batch.MoreLink = inboxURL
	}
	return batch, true
}

// FormatBatch renders one message: one line per item "<headline>
// <link>", then "+N more → <inboxURL>" when More > 0.
func FormatBatch(b Batch) string {
	lines := make([]string, 0, len(b.Items)+1)
	for _, it := range b.Items {
		lines = append(lines, it.Headline+" "+it.Link)
	}
	if b.More > 0 {
		lines = append(lines, fmt.Sprintf("+%d more → %s", b.More, b.MoreLink))
	}
	return strings.Join(lines, "\n")
}

// inQuietHours reports whether the local clock time of now falls inside
// the quiet window. Minutes are read from now's own location, so the
// window follows wall-clock time across DST transitions. An empty
// window (QuietStart == QuietEnd) is never quiet.
func inQuietHours(now time.Time, p DeliveryPrefs) bool {
	if p.QuietStart == p.QuietEnd {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if p.QuietStart < p.QuietEnd {
		return m >= p.QuietStart && m < p.QuietEnd
	}
	return m >= p.QuietStart || m < p.QuietEnd
}

// normalizePrefs fills unset numeric fields from DefaultDeliveryPrefs:
// a zero interval, item cap, or an all-zero quiet window means default.
func normalizePrefs(p DeliveryPrefs) DeliveryPrefs {
	d := DefaultDeliveryPrefs()
	if p.Interval <= 0 {
		p.Interval = d.Interval
	}
	if p.MaxItems <= 0 {
		p.MaxItems = d.MaxItems
	}
	if p.QuietStart == 0 && p.QuietEnd == 0 {
		p.QuietStart, p.QuietEnd = d.QuietStart, d.QuietEnd
	}
	return p
}
