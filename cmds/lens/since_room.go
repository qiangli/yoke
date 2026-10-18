package lenscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/room"
)

// The bus is carried by the room timeline. Its source token is the same one
// consumed by quota immune, wrapped by lens in the bound --since envelope.
type busTimelineDelta struct{}

func (busTimelineDelta) Delta(ctx context.Context, loc ref.Locator, token string) ([]byte, string, string, error) {
	if loc.Target != "timeline" {
		return nil, "", "", ErrDeltaExpired
	}
	events, next, err := room.ReadDelta(ctx, token)
	if errors.Is(err, room.ErrDeltaExpired) {
		return nil, "", "", ErrDeltaExpired
	}
	if err != nil {
		return nil, "", "", err
	}
	var b bytes.Buffer
	for _, e := range events {
		if e.Type != room.EventNotify {
			continue
		}
		line, err := json.Marshal(e)
		if err != nil {
			return nil, "", "", err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes(), next, "", nil
}

func init() { _ = RegisterDeltaSource("bus", busTimelineDelta{}) }
