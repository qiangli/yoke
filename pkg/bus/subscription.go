package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/room"
)

// Delivery tiers. The distinction is the whole reason a bus for agents is harder
// than a bus for services: an agent mid-turn cannot read a queue, and an agent
// that is interrupted for something trivial is worse off than one left alone.
const (
	// DeliveryQueued lands in the pending buffer, to be read at a turn boundary.
	// A course-correction, not an interruption.
	DeliveryQueued = "queued"
	// DeliveryInterrupt additionally breaks into a running turn (ESC + text over
	// the control socket). Reserved for direction-changers.
	DeliveryInterrupt = "interrupt"
)

const subsDir = "subs"

// Subscription is one agent's standing interest, held by the sidecar on its
// behalf.
//
// The agent does not poll and does not evaluate any of this: it reads a
// pre-resolved buffer. Everything here exists so the sidecar can decide, OFF the
// agent's critical path, whether a given event is relevant and how urgently it
// should arrive.
type Subscription struct {
	Subscriber string `json:"subscriber"`
	// Topics are exact (`code.api.Foo`), dotted prefixes (`code.api.*`), or `*`
	// for everything.
	//
	// They are also this reader's DECLARED CONCERNS on the message board: a
	// board post whose Topic matches one of them is routed into the reader's
	// uncapped tier (see Post.OnConcern and Unseen in board.go). One field,
	// one declaration — a subscription that routed bus events and a separate
	// concern list that routed board posts would be two spellings of one
	// interest, free to drift.
	Topics []string `json:"topics,omitempty"`
	// To also accepts 1:1 notifications addressed to this id.
	To string `json:"to,omitempty"`
	// Room also accepts room-scoped notifications for this room.
	Room string `json:"room,omitempty"`

	// Instance is the room member id to steer when an interrupt is authorized.
	// Without it, an interrupt has nowhere to go and degrades to queued.
	Instance string `json:"instance,omitempty"`

	// InterruptFrom lists the principals whose interrupts this subscriber will
	// honour. EMPTY MEANS NOBODY.
	//
	// This is the governance boundary, and defaulting it closed is deliberate:
	// the power to break into an agent's turn is an attack surface, so it is
	// granted by name rather than assumed. Everyone may still REPORT (queued);
	// only named principals may AUTHOR a redirect — the same report/author split
	// the fleet uses elsewhere.
	InterruptFrom []string `json:"interrupt_from,omitempty"`

	// MaxPerMin caps how many notifications this subscriber is handed per minute
	// before the rest are demoted to queued. Zero means the default.
	MaxPerMin int `json:"max_per_min,omitempty"`

	// Since is the sidecar's read offset into the timeline.
	Since int64 `json:"since,omitempty"`
}

// DefaultMaxPerMin bounds interrupt delivery.
//
// Pollution is the single biggest failure mode of a bus for agents — steering
// quality collapses as messages multiply — so the cap is low and deliberately
// unimpressive. Exceeding it never DROPS anything; it demotes the surplus to
// queued, where it is still read at the next turn boundary.
const DefaultMaxPerMin = 3

func (s Subscription) maxPerMin() int {
	if s.MaxPerMin > 0 {
		return s.MaxPerMin
	}
	return DefaultMaxPerMin
}

// Matches reports whether an event is one this subscription asked for.
//
// Only notify events ever match: the timeline also carries join/leave/steer
// bookkeeping, which is addressed to nobody and would be pure noise on a bus.
func (s Subscription) Matches(e room.Event) bool {
	if e.Type != room.EventNotify {
		return false
	}
	if s.To != "" && e.To == s.To {
		return true
	}
	if s.Room != "" && e.Room == s.Room {
		return true
	}
	for _, pat := range s.Topics {
		if topicMatch(pat, e.Topic) {
			return true
		}
	}
	return false
}

// topicMatch implements exact and dotted-prefix topic matching.
//
// Topics are hierarchical (`code.api.Foo`, `task.123.done`), so `code.api.*`
// must match `code.api.Foo` and `code.api.Foo.renamed` but NOT `code.apiary` —
// the wildcard covers whole dotted segments, not arbitrary characters. Getting
// that wrong turns a precise subscription into a broad one, which is exactly the
// pollution the design warns about.
func topicMatch(pattern, topic string) bool {
	if pattern == "" || topic == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if pattern == topic {
		return true
	}
	prefix, ok := strings.CutSuffix(pattern, "*")
	if !ok {
		return false
	}
	prefix = strings.TrimSuffix(prefix, ".")
	if prefix == "" {
		return true
	}
	// Segment-anchored: the topic must BE the prefix or continue it with a dot.
	return topic == prefix || strings.HasPrefix(topic, prefix+".")
}

// AcceptsInterruptFrom reports whether this subscriber honours an interrupt from
// a given principal.
func (s Subscription) AcceptsInterruptFrom(principal string) bool {
	if principal == "" {
		return false
	}
	for _, p := range s.InterruptFrom {
		if p == principal || p == "*" {
			return true
		}
	}
	return false
}

// --- concerns: the declaration half of the board's subject route ------------

// DeclaredConcerns is the set of topic patterns this reader has declared an
// interest in — its subscription's Topics, plus ConcernAnnounce, the board's
// wall, which every reader is subscribed to by default. A reader with no
// subscription at all still hears announcements; everything else stays behind
// the cap until it is asked for by name.
//
// Declaring is done with the subscription front door the topics already have:
// `bashy bus subscribe --topic shared-baseline`. Remember the convention it
// buys into: a declared concern is an obligation to READ it.
func DeclaredConcerns(reader string) []string {
	var out []string
	if s, err := LoadSubscription(reader); err == nil {
		out = s.Topics
	}
	for _, t := range out {
		if topicMatch(t, ConcernAnnounce) {
			return out
		}
	}
	return append(out, ConcernAnnounce)
}

// ConcernDeclarers names the subscribers whose declared topics cover a
// concern — the denominator for "did everyone concerned read it", answered
// against the counted views the ModeAll machinery already keeps. Implicit
// announce subscribers are not listed: the explicit declarations are the only
// ones that carry the obligation to read.
func ConcernDeclarers(topic string) []string {
	subs, err := Subscriptions()
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range subs {
		for _, pat := range s.Topics {
			if topicMatch(pat, topic) {
				out = append(out, s.Subscriber)
				break
			}
		}
	}
	return out
}

// --- storage --------------------------------------------------------------

func subsPath(subscriber string) (string, error) {
	name := safeName.ReplaceAllString(subscriber, "_")
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "anonymous"
	}
	dir := filepath.Join(room.Dir(), subsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("bus: creating %s: %w", dir, err)
	}
	return filepath.Join(dir, name+".json"), nil
}

// SaveSubscription writes (or replaces) a subscription.
func SaveSubscription(s Subscription) error {
	path, err := subsPath(s.Subscriber)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// LoadSubscription reads one subscription.
func LoadSubscription(subscriber string) (Subscription, error) {
	var s Subscription
	path, err := subsPath(subscriber)
	if err != nil {
		return s, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, fmt.Errorf("bus: %q is not subscribed", subscriber)
		}
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("bus: subscription for %q is unreadable: %w", subscriber, err)
	}
	return s, nil
}

// Subscriptions lists every subscription, sorted by subscriber.
func Subscriptions() ([]Subscription, error) {
	dir := filepath.Join(room.Dir(), subsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Subscription
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var s Subscription
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subscriber < out[j].Subscriber })
	return out, nil
}

// RemoveSubscription tears a subscription down. Subscriptions are meant to be
// scoped to a piece of work — set up when an agent starts an issue, removed when
// it finishes — so that a finished agent stops being a delivery target.
func RemoveSubscription(subscriber string) error {
	path, err := subsPath(subscriber)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("bus: %q is not subscribed", subscriber)
		}
		return err
	}
	return nil
}
