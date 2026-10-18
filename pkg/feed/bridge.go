package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// Bridge routes envelopes from one channel+conversation to another.
//
// A route maps (SrcChannel, SrcConversation) -> (DstChannel, DstConversation).
// One-way by default; TwoWay creates the reverse mapping as well.
// Inbound envelopes matching a route are re-sent through the target adapter
// with attribution ("[sender via origin] text"), attachments and reply
// threading where supported. Loop prevention uses OriginChain + message-id
// dedupe. Only paired senders are bridged. DMs are never bridged unless
// AllowDM is set. Per-route rate limits throttle forwarding.

const bridgesFile = "bridges.json"

// BridgeRoute is one directed bridge edge.
type BridgeRoute struct {
	ID              string `json:"id"`
	SrcChannel      string `json:"src_channel"`
	SrcConversation string `json:"src_conversation"`
	DstChannel      string `json:"dst_channel"`
	DstConversation string `json:"dst_conversation"`
	TwoWay          bool   `json:"two_way,omitempty"`
	AllowDM         bool   `json:"allow_dm,omitempty"`
	RateLimitPerMin int    `json:"rate_limit_per_min,omitempty"` // 0 = unlimited
	CreatedAt       string `json:"created_at"`
}

// bridgesPath returns the bridges file path.
func bridgesPath(dir string) string { return filepath.Join(dir, bridgesFile) }

func validateBridgeRoute(r BridgeRoute) error {
	if strings.TrimSpace(r.SrcChannel) == "" {
		return errors.New("feed: bridge src_channel is required")
	}
	if strings.TrimSpace(r.SrcConversation) == "" {
		return errors.New("feed: bridge src_conversation is required")
	}
	if strings.TrimSpace(r.DstChannel) == "" {
		return errors.New("feed: bridge dst_channel is required")
	}
	if strings.TrimSpace(r.DstConversation) == "" {
		return errors.New("feed: bridge dst_conversation is required")
	}
	if r.SrcChannel == r.DstChannel && r.SrcConversation == r.DstConversation {
		return errors.New("feed: bridge src and dst are the same conversation")
	}
	if r.RateLimitPerMin < 0 {
		return errors.New("feed: bridge rate_limit_per_min must not be negative")
	}
	return nil
}

// LoadBridges reads dir/bridges.json. Missing file means none.
func LoadBridges(dir string) ([]BridgeRoute, error) {
	data, err := os.ReadFile(bridgesPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed: load bridges: %w", err)
	}
	var rs []BridgeRoute
	if err := json.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("feed: load bridges: %w", err)
	}
	for _, r := range rs {
		if err := validateBridgeRoute(r); err != nil {
			return nil, err
		}
	}
	return rs, nil
}

// SaveBridges validates and writes dir/bridges.json atomically, 0600.
func SaveBridges(dir string, rs []BridgeRoute) error {
	for _, r := range rs {
		if err := validateBridgeRoute(r); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, r := range rs {
		if seen[r.ID] {
			return fmt.Errorf("feed: duplicate bridge id %q", r.ID)
		}
		seen[r.ID] = true
	}
	if rs == nil {
		rs = []BridgeRoute{}
	}
	return writeJSON0600(dir, bridgesPath(dir), rs)
}

// BridgeEngine routes inbound envelopes through configured bridges.
type BridgeEngine struct {
	dir      string
	mu       sync.Mutex
	seen     map[string]bool // message-id dedupe
	rate     map[string][]time.Time
	pairing  PairingChecker
	channels func() ([]Channel, error)
	now      func() time.Time
}

// NewBridgeEngine creates a bridge engine. channels supplies current channel
// configs (for outbound delivery). pairing checks sender allowlist.
func NewBridgeEngine(dir string, channels func() ([]Channel, error), pairing PairingChecker) *BridgeEngine {
	if pairing == nil {
		pairing = AllowAllPairing{}
	}
	return &BridgeEngine{
		dir:      dir,
		seen:     map[string]bool{},
		rate:     map[string][]time.Time{},
		pairing:  pairing,
		channels: channels,
		now:      time.Now,
	}
}

// Handle processes one inbound envelope: matches bridges, checks policy,
// prevents loops, and forwards to the destination adapter.
func (b *BridgeEngine) Handle(ctx context.Context, env Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	if !b.pairing.IsPaired(env.SenderID) {
		return ErrUnpairedSender
	}
	if env.IsDM {
		// DM bridging requires explicit AllowDM on the matched route;
		// handled per-route below. If no route allows it, drop.
	}

	b.mu.Lock()
	key := env.OriginKey()
	if b.seen[key] {
		b.mu.Unlock()
		return nil // dedupe
	}
	b.seen[key] = true
	b.mu.Unlock()

	routes, err := LoadBridges(b.dir)
	if err != nil {
		return err
	}

	// Find matching routes.
	var matched []BridgeRoute
	for _, r := range routes {
		if matchesRoute(env, r) {
			matched = append(matched, r)
		}
		// TwoWay routes also match the reverse direction.
		if r.TwoWay && matchesReverse(env, r) {
			rev := BridgeRoute{
				ID: r.ID + ":reverse", SrcChannel: r.DstChannel, SrcConversation: r.DstConversation,
				DstChannel: r.SrcChannel, DstConversation: r.SrcConversation,
				AllowDM: r.AllowDM, RateLimitPerMin: r.RateLimitPerMin,
			}
			matched = append(matched, rev)
		}
	}

	for _, r := range matched {
		if env.IsDM && !r.AllowDM {
			continue
		}
		if ChainContains(env.OriginChain, r.DstChannel, r.DstConversation) {
			continue // loop prevention
		}
		if !b.allowRate(r.ID) {
			continue
		}
		if err := b.forward(ctx, env, r); err != nil {
			// One route failure does not block others.
			continue
		}
	}
	return nil
}

func matchesRoute(env Envelope, r BridgeRoute) bool {
	return env.Channel == r.SrcChannel && env.ConversationID == r.SrcConversation
}
func matchesReverse(env Envelope, r BridgeRoute) bool {
	return env.Channel == r.DstChannel && env.ConversationID == r.DstConversation
}

func (b *BridgeEngine) allowRate(routeID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	route, err := b.findRoute(routeID)
	if err != nil || route.RateLimitPerMin <= 0 {
		return true
	}
	now := b.now()
	window := now.Add(-time.Minute)
	times := b.rate[routeID]
	// Prune outside window.
	keep := times[:0]
	for _, t := range times {
		if t.After(window) {
			keep = append(keep, t)
		}
	}
	b.rate[routeID] = keep
	if len(keep) >= route.RateLimitPerMin {
		return false
	}
	b.rate[routeID] = append(keep, now)
	return true
}

func (b *BridgeEngine) findRoute(id string) (BridgeRoute, error) {
	routes, err := LoadBridges(b.dir)
	if err != nil {
		return BridgeRoute{}, err
	}
	base := strings.TrimSuffix(id, ":reverse")
	for _, r := range routes {
		if r.ID == base {
			return r, nil
		}
	}
	return BridgeRoute{}, fmt.Errorf("bridge %q not found", id)
}

func (b *BridgeEngine) forward(ctx context.Context, env Envelope, r BridgeRoute) error {
	if b.channels == nil {
		return errors.New("feed: no channel supplier")
	}
	channels, err := b.channels()
	if err != nil {
		return err
	}
	var dst *Channel
	for i, ch := range channels {
		if ch.Name == r.DstChannel {
			dst = &channels[i]
			break
		}
	}
	if dst == nil {
		return fmt.Errorf("feed: bridge dst channel %q not found", r.DstChannel)
	}
	// Build forwarded envelope with attribution.
	fwd := Envelope{
		Version:        ContractVersion,
		ID:             env.ID,
		Channel:        r.DstChannel,
		MessageID:      env.MessageID,
		ConversationID: r.DstConversation,
		SenderID:       env.SenderID,
		SenderDisplay:  env.SenderDisplay,
		Text:           attributionText(env),
		Attachments:    env.Attachments,
		ReplyTo:        env.ReplyTo,
		ReceivedAt:     env.ReceivedAt,
		OriginChain:    append(append([]string(nil), env.OriginChain...), env.Channel+":"+env.ConversationID),
		IsDM:           false,
	}
	_, err = SendEnvelopes(ctx, *dst, []Envelope{fwd})
	return err
}

func attributionText(env Envelope) string {
	display := env.SenderDisplay
	if display == "" {
		display = env.SenderID
	}
	return fmt.Sprintf("[%s via %s] %s", display, env.Channel, env.Text)
}

// ---- CLI: bashy feed bridge add/list/remove ----

func newBridgeCmd(env Env) *cobra.Command {
	bridge := &cobra.Command{
		Use:   "bridge",
		Short: "manage bridge routes between channel conversations",
		Long: `A bridge route maps conversation A on channel X to conversation B on
channel Y. Inbound envelopes matching a route are re-sent through the
target adapter with attribution, attachments and reply threading where
supported. Loop prevention uses origin chain + message-id dedupe. Only
paired senders are bridged; DMs never cross unless --allow-dm. Per-route
rate limits throttle forwarding.

  bashy feed bridge add SRC_CHANNEL:CONVERSATION DST_CHANNEL:CONVERSATION [--two-way] [--allow-dm] [--rate-limit N]
  bashy feed bridge list
  bashy feed bridge rm ID`,
	}
	var (
		twoWay    bool
		allowDM   bool
		rateLimit int
	)
	add := &cobra.Command{
		Use:   "add SRC DST",
		Short: "add a bridge route",
		Args:  cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, a []string) error {
			srcCh, srcConv, err := parseRoutePart(a[0])
			if err != nil {
				return err
			}
			dstCh, dstConv, err := parseRoutePart(a[1])
			if err != nil {
				return err
			}
			dir := env.dir()
			routes, err := LoadBridges(dir)
			if err != nil {
				return err
			}
			id := fmt.Sprintf("%s:%s->%s:%s", srcCh, srcConv, dstCh, dstConv)
			// Check duplicate
			for _, r := range routes {
				if r.ID == id {
					return fmt.Errorf("feed: bridge %q already exists", id)
				}
			}
			r := BridgeRoute{
				ID: id, SrcChannel: srcCh, SrcConversation: srcConv,
				DstChannel: dstCh, DstConversation: dstConv,
				TwoWay: twoWay, AllowDM: allowDM, RateLimitPerMin: rateLimit,
				CreatedAt: env.now().UTC().Format(time.RFC3339),
			}
			if err := validateBridgeRoute(r); err != nil {
				return err
			}
			if err := SaveBridges(dir, append(routes, r)); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added bridge %s\n", r.ID)
			return nil
		},
	}
	add.Flags().BoolVar(&twoWay, "two-way", false, "create a two-way bridge (also B->A)")
	add.Flags().BoolVar(&allowDM, "allow-dm", false, "allow bridging direct messages")
	add.Flags().IntVar(&rateLimit, "rate-limit", 0, "max forwards per minute (0 = unlimited)")

	list := &cobra.Command{
		Use:   "list",
		Short: "list bridge routes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			routes, err := LoadBridges(env.dir())
			if err != nil {
				return err
			}
			sort.Slice(routes, func(i, j int) bool { return routes[i].ID < routes[j].ID })
			for _, r := range routes {
				line := fmt.Sprintf("%s\t%s:%s -> %s:%s", r.ID, r.SrcChannel, r.SrcConversation, r.DstChannel, r.DstConversation)
				if r.TwoWay {
					line += "\ttwo-way"
				}
				if r.AllowDM {
					line += "\tallow-dm"
				}
				if r.RateLimitPerMin > 0 {
					line += fmt.Sprintf("\trate=%d/min", r.RateLimitPerMin)
				}
				fmt.Fprintln(c.OutOrStdout(), line)
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use:   "rm ID",
		Short: "remove a bridge route",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, a []string) error {
			dir := env.dir()
			routes, err := LoadBridges(dir)
			if err != nil {
				return err
			}
			var keep []BridgeRoute
			for _, r := range routes {
				if r.ID != a[0] {
					keep = append(keep, r)
				}
			}
			if len(keep) == len(routes) {
				return fmt.Errorf("feed: no bridge named %q", a[0])
			}
			if err := SaveBridges(dir, keep); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed bridge %s\n", a[0])
			return nil
		},
	}
	bridge.AddCommand(add, list, rm)
	return bridge
}

func parseRoutePart(s string) (channel, conversation string, err error) {
	ch, conv, ok := strings.Cut(s, ":")
	if !ok || ch == "" || conv == "" {
		return "", "", fmt.Errorf("feed: bridge route part %q must be CHANNEL:CONVERSATION", s)
	}
	return ch, conv, nil
}
