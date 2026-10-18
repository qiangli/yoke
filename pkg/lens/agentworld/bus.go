package agentworld

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/room"
)

const busTimeline = "bus://timeline"

// TimelineDelta is the bus://timeline change source shared by lens --since
// and readers that checkpoint the same source independently.
func TimelineDelta(ctx context.Context, token string) ([]room.Event, string, error) {
	events, next, err := room.ReadDelta(ctx, token)
	if err != nil {
		return nil, "", err
	}
	kept := events[:0]
	for _, event := range events {
		if event.Type == room.EventNotify || event.Type == room.EventUsage && event.Topic == room.TopicQuotaObservation {
			kept = append(kept, event)
		}
	}
	return kept, next, nil
}

// ReadTimeline resumes a bound --since cursor. A pre-envelope room token is
// accepted so watchdog state written by older versions can be resumed.
func ReadTimeline(ctx context.Context, since string) ([]room.Event, string, error) {
	token := since
	if strings.HasPrefix(strings.TrimSpace(since), "{") {
		cursor, err := stream.ParseCursor([]byte(since))
		if err != nil {
			return nil, "", err
		}
		if err := cursor.Check("bus", busTimeline, "delta:jsonl"); err != nil {
			return nil, "", err
		}
		if cursor.Kind != stream.KindChangeStream {
			return nil, "", stream.ErrCursorBinding
		}
		if cursor.Expiry != "" {
			expiry, _ := time.Parse(time.RFC3339, cursor.Expiry)
			if !time.Now().Before(expiry) {
				return nil, "", room.ErrDeltaExpired
			}
		}
		token = cursor.Token
	}
	events, next, err := TimelineDelta(ctx, token)
	if err != nil {
		return nil, "", err
	}
	cursor := stream.Cursor{SchemaVersion: stream.CursorSchema, Kind: stream.KindChangeStream, Token: next,
		Binding: stream.Binding{Provider: "bus", Target: busTimeline, Query: "delta:jsonl"}}
	if err := cursor.Validate(); err != nil {
		return nil, "", err
	}
	b, err := json.Marshal(cursor)
	if err != nil {
		return nil, "", err
	}
	return events, string(b), nil
}

// IsExpired covers the source's rotation and a cursor whose binding changed.
func IsExpired(err error) bool {
	return errors.Is(err, room.ErrDeltaExpired) || errors.Is(err, stream.ErrCursorBinding)
}
