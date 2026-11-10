// Package outpostagent contains the shared interpretation of Outpost's
// agent.json fields used by yoke.
package outpostagent

import "strings"

// PortalURL returns the canonical portal URL when agent.json provides one,
// otherwise fallback. Callers supply their existing legacy fallback because
// older agent.json consumers intentionally used different representations.
func PortalURL(portalURL, fallback string) string {
	if portalURL = strings.TrimSpace(portalURL); portalURL != "" {
		return portalURL
	}
	return fallback
}
