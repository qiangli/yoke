package lenscmd

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/room"
)

func init() {
	if err := RegisterDeltaSource("bus", &busDeltaSource{}); err != nil {
		panic(err)
	}
}

type busDeltaSource struct{}

func (b *busDeltaSource) TranslateSeq(ctx context.Context, loc ref.Locator, seq int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(loc.Target) == "timeline" {
		return "", fmt.Errorf("lens: bus: timeline target does not support sequence in --since")
	}
	if seq < 1 {
		return "", fmt.Errorf("lens: bus: sequence must be positive")
	}
	floor := room.ArchivedThrough()
	reader := room.NewTimelineReader(nil)
	pos, err := reader.Latest(ctx)
	if err != nil {
		return "", err
	}
	head := pos.Seq
	if head < floor {
		head = floor
	}
	if seq < floor || seq > head {
		return "", ErrDeltaExpired
	}
	return strconv.FormatInt(seq, 10), nil
}

func (b *busDeltaSource) TranslateTime(ctx context.Context, loc ref.Locator, t time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(loc.Target) == "timeline" {
		return "", fmt.Errorf("lens: bus: timeline target does not support time/duration in --since")
	}
	// A sequence boundary cannot represent a time predicate: producers can
	// supply timestamps out of order. Carry the cutoff into Delta and filter
	// authoritative event timestamps there.
	return "time:" + t.UTC().Format(time.RFC3339Nano), nil
}

func parseEventTS(ts string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse event timestamp %q", ts)
}

func (b *busDeltaSource) Delta(ctx context.Context, loc ref.Locator, token string) ([]byte, string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", "", err
	}
	if loc.Scheme != "bus" {
		return nil, "", "", fmt.Errorf("lens: delta source for bus cannot serve %q", loc.Scheme)
	}
	if len(loc.Inner) != 0 {
		return nil, "", "", fmt.Errorf("lens: bus: container descent (!/) is not supported for records: %w", provider.ErrUnsupported)
	}
	if loc.ScopeKind != "" {
		return nil, "", "", fmt.Errorf("lens: bus: location scopes are not supported for records yet: %w", provider.ErrUnsupported)
	}

	target := strings.TrimSpace(loc.Target)
	if target == "timeline" {
		return busTimelineDelta{}.Delta(ctx, loc, token)
	}
	var (
		matchKind string
		matchSeq  int64 = -1
	)
	switch target {
	case "", "events", "all", "*":
		// match all events
	default:
		clean := strings.TrimPrefix(target, "#")
		if seq, err := strconv.ParseInt(clean, 10, 64); err == nil {
			if seq < 1 {
				return nil, "", "", fmt.Errorf("lens: bus: %q is not a positive sequence", target)
			}
			matchSeq = seq
		} else if isKnownEventKind(target) {
			matchKind = target
		} else {
			return nil, "", "", fmt.Errorf("lens: bus: %q is not a valid sequence or events target", target)
		}
	}

	floor := room.ArchivedThrough()
	reader := room.NewTimelineReader(nil)
	pos, err := reader.Latest(ctx)
	if err != nil {
		return nil, "", "", err
	}
	head := pos.Seq
	if head < floor {
		head = floor
	}

	if matchSeq > 0 && matchSeq <= floor {
		return nil, "", "", ErrDeltaExpired
	}

	var after int64
	var explicit bool
	var cutoff time.Time
	timeQuery := strings.HasPrefix(token, "time:")
	if timeQuery {
		cutoff, err = parseEventTS(strings.TrimPrefix(token, "time:"))
		if err != nil {
			return nil, "", "", err
		}
		// Archived timestamps are unavailable; we cannot prove a complete answer.
		if floor > 0 {
			return nil, "", "", ErrDeltaExpired
		}
		token = ""
		explicit = true
	}
	if token != "" {
		after, err = parseResumeToken(token)
		if err != nil {
			return nil, "", "", err
		}
		explicit = true
		if after < floor || after > head {
			return nil, "", "", ErrDeltaExpired
		}
		if matchSeq > 0 && after >= matchSeq {
			return []byte{}, strconv.FormatInt(head, 10), "", nil
		}
	}

	var records bytes.Buffer
	highestDelivered := after

	matchFn := func(e room.Event) bool {
		if matchSeq > 0 && e.Seq != matchSeq {
			return false
		}
		if matchKind != "" && e.Type != matchKind {
			return false
		}
		return true
	}

	opt := room.StreamOptions{
		After:    after,
		Explicit: explicit,
		Follow:   false,
	}

	err = room.Stream(ctx, opt, func(it room.StreamItem) error {
		if it.Resync != nil {
			return ErrDeltaExpired
		}
		if timeQuery {
			ts, err := parseEventTS(it.Event.TS)
			if err != nil {
				return err
			}
			if ts.Before(cutoff) {
				return nil
			}
		}
		if !matchFn(it.Event) {
			return nil
		}
		wire, err := bus.EncodeStreamItem(it)
		if err != nil {
			return err
		}
		records.Write(wire)
		records.WriteByte('\n')
		if it.Event.Seq > highestDelivered {
			highestDelivered = it.Event.Seq
		}
		return nil
	})
	if err != nil {
		return nil, "", "", err
	}

	nextSeq := head
	if highestDelivered > nextSeq {
		nextSeq = highestDelivered
	}
	if nextSeq < after {
		nextSeq = after
	}

	return records.Bytes(), strconv.FormatInt(nextSeq, 10), "", nil
}

func parseResumeToken(token string) (int64, error) {
	if token == "" {
		return 0, fmt.Errorf("invalid resume token: empty")
	}
	for i := 0; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return 0, fmt.Errorf("invalid resume token %q: must be unsigned decimal", token)
		}
	}
	seq, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid resume token %q: %w", token, err)
	}
	return seq, nil
}

func isKnownEventKind(k string) bool {
	switch k {
	case room.EventSprint, room.EventHandoff, room.EventTodo,
		"join", "leave", "steer", "interrupt", "status", "note", "notify", "approval", "usage", "inbound":
		return true
	}
	return false
}
