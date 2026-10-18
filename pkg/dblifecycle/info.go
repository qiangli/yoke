package dblifecycle

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/ref"
)

type TargetInfo struct {
	Locator  string       `json:"locator,omitempty"`
	ID       string       `json:"id"`
	Engine   string       `json:"engine"`
	Category string       `json:"category,omitempty"`
	Mode     dbstore.Mode `json:"mode"`
	Lifetime string       `json:"lifetime"`
}

func ViewTarget(id dbstore.Identity, locator string) TargetInfo {
	if locator == "" && strings.HasPrefix(id.ID, "db:") {
		locator = (ref.Locator{Scheme: "db", Target: strings.TrimPrefix(id.ID, "db:")}).String()
	}
	lifetime := "persistent"
	if id.Mode == dbstore.Memory {
		lifetime = "owner"
	}
	category := ""
	if a, err := Engine(id.Engine); err == nil {
		cats := a.Caps().Categories
		if len(cats) > 0 {
			category = cats[0]
		}
	}
	return TargetInfo{Locator: locator, ID: id.ID, Engine: id.Engine, Category: category, Mode: id.Mode, Lifetime: lifetime}
}

// Show reads the target through the same admission/claim guard as check and
// category operations. Size is a file size only; directories report unknown.
func (s Service) Show(ctx context.Context, t dbstore.Target, wait time.Duration) (dbstore.Identity, map[string]any, error) {
	h, err := s.Open(ctx, t, true, wait)
	if err != nil {
		return dbstore.Identity{}, nil, err
	}
	defer h.Close()
	result, err := showHandle(h)
	return h.Identity(), result, err
}

func showHandle(h dbstore.Handle) (map[string]any, error) {
	id := h.Identity()
	result := map[string]any{"id": id.ID, "engine": id.Engine, "mode": id.Mode, "lifetime": ViewTarget(id, "").Lifetime, "size_bytes": nil, "schema_summary": nil, "index_freshness": nil, "holders": []any{}, "sessions": []string{}}
	if id.Mode == dbstore.Persistent {
		info, err := os.Stat(id.Path)
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			result["size_bytes"] = info.Size()
		}
	} else {
		result["sessions"] = []string{id.Session}
	}
	claims, err := coord.List(coord.DefaultDir())
	if err != nil {
		return nil, err
	}
	var holders []any
	for _, claim := range claims {
		if claim != nil && claim.Address().Kind == "db" && claim.Resource == id.ID {
			holders = append(holders, map[string]any{"holder": claim.Holder.Name, "episode": claim.Holder.Episode, "epoch": claim.Epoch, "scope": "database", "mode": claim.Mode})
		}
	}
	result["holders"] = holders
	return result, nil
}
