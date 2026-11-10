// Package outpostagent contains the shared interpretation of Outpost's
// agent.json fields used by yoke.
package outpostagent

import (
	"net"
	"net/url"
	"strings"
)

// PortalURL returns the canonical portal URL when agent.json provides one,
// otherwise fallback. Callers supply their existing legacy fallback because
// older agent.json consumers intentionally used different representations.
func PortalURL(portalURL, fallback string) string {
	if portalURL = strings.TrimSpace(portalURL); portalURL != "" {
		return portalURL
	}
	return fallback
}

// Cloudbox is agent.json's `cloudbox` block: the host's ONE pairing with the
// cloudbox master (identity only — no tunnel). See
// docs/cloudbox-members-attach-wire.md §3.
type Cloudbox struct {
	PortalURL  string   `json:"portal_url"`
	Host       string   `json:"host"`
	Token      string   `json:"token"`
	Scopes     []string `json:"scopes,omitempty"`
	OwnerEmail string   `json:"owner_email,omitempty"`
}

// Attachment is one row of agent.json's `attachments`: a satellite this host
// is attached to as a member of its account, with the satellite host token
// that satellite issued (wire §4). Host is this host's name on the satellite
// and Tunnel the matrix-tunnel registration the attach returned.
type Attachment struct {
	SatelliteID string            `json:"satellite_id"`
	Name        string            `json:"name,omitempty"`
	Endpoint    string            `json:"endpoint"`
	Host        string            `json:"host,omitempty"`
	Token       string            `json:"token"`
	Scopes      []string          `json:"scopes,omitempty"`
	Tunnel      *AttachmentTunnel `json:"tunnel,omitempty"`
}

// AttachmentTunnel is the subset of an attachment's tunnel registration yoke
// reads: the agent name the satellite serves this host under.
type AttachmentTunnel struct {
	AgentName string `json:"agent_name,omitempty"`
}

// Matches reports whether the attachment is the satellite called name, by
// name or satellite id (case-insensitive).
func (a Attachment) Matches(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && (strings.EqualFold(strings.TrimSpace(a.Name), name) || strings.EqualFold(strings.TrimSpace(a.SatelliteID), name))
}

// AppURL is the URL of this host's app on the attached satellite:
// <endpoint>/matrix/h/<host or tunnel agent_name>/app/<app>. It returns ""
// when the attachment names no endpoint or host.
func (a Attachment) AppURL(app string) string {
	ep := strings.TrimRight(strings.TrimSpace(a.Endpoint), "/")
	host := strings.TrimSpace(a.Host)
	if host == "" && a.Tunnel != nil {
		host = strings.TrimSpace(a.Tunnel.AgentName)
	}
	if ep == "" || host == "" {
		return ""
	}
	return ep + "/matrix/h/" + url.PathEscape(host) + "/app/" + url.PathEscape(app)
}

// Paired reports whether the block holds a usable cloudbox identity.
func (c *Cloudbox) Paired() bool {
	return c != nil && strings.TrimSpace(c.Token) != ""
}

// Origin normalizes a URL to scheme://host[:port] with the default port
// dropped, for matching a credential to the server that issued it. Unlike a
// bare hostname compare it keeps the port: two servers on one host are two
// servers. A value without a scheme is taken as https.
func Origin(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host
}
