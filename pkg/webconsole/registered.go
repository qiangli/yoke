// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"fmt"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Registered apps (`bashy app add`) are the rod beside the stock panels: any
// local web server the operator names, proxied at /<name>/. The record is the
// dhnt-app-meta-v1 contract persisted in the fleet ring, so discovery READS
// data and never execs — the rule --app has to spend a probe to honor.

// appMeta turns a registered record into the contract Validate checks. The
// record's auth IS operator policy (the operator wrote it), so unlike a probed
// binary's self-description it is kept, not forced to system.
func appMeta(a fleet.App) AppMeta {
	m := AppMeta{
		SchemaVersion: MetaSchema,
		Name:          a.Name,
		Label:         a.Label,
		Icon:          a.Icon,
		Tip:           a.Tip,
		Mount:         a.Name,
		Mode:          atlas.WebProxy,
		Port:          a.Port,
		Start:         a.Start,
		Auth:          a.Auth,
		LoginPath:     a.LoginPath,
	}
	if m.Label == "" {
		m.Label = m.Name
	}
	if m.Auth == "" {
		m.Auth = AuthSystem
	}
	if m.Auth != AuthCustom {
		m.LoginPath = ""
	}
	return m
}

// ValidateRegisteredApp is the check `bashy app add`/`set` wire into the
// fleet store (fleet.WithAppValidate): the same one discovery applies, against
// the mounts bashy ships.
func ValidateRegisteredApp(a fleet.App) error {
	return appMeta(a).Validate(TakenMounts(stockPanels()))
}

// discoverRegistered reads the registered-app ring into proxy panels. A bad
// or colliding record is reported and skipped; the launcher still comes up.
func discoverRegistered(taken map[string]bool) ([]Panel, []error) {
	apps, errs := fleet.New().Apps()
	var out []Panel
	for _, a := range apps {
		m := appMeta(a)
		if err := m.Validate(taken); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
			continue
		}
		taken[m.Mount] = true
		out = append(out, Panel{
			Name:        m.Mount,
			Label:       m.Label,
			Path:        "/" + m.Mount + "/",
			Mode:        atlas.WebProxy,
			Port:        m.Port,
			Start:       m.Start,
			StartIsFull: true,
			Icon:        m.Icon,
			Tip:         m.Tip,
			Auth:        m.Auth,
			LoginPath:   m.LoginPath,
			Source:      "registered",
			Available:   true,
		})
	}
	return out, errs
}
