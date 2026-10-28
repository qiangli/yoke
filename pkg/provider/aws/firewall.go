// The satellite firewall (S522.2 item 8): the Lightsail
// PutInstancePublicPorts rule set applied right after create, mirroring
// the DigitalOcean `dhnt-satellite` cloud firewall.
//
// PutInstancePublicPorts REPLACES the instance's whole public-port set, so
// the rule list here is the complete intended state, not a delta — including
// SSH, which is deliberately NOT open to the internet: port 22 is reachable
// only through the `lightsail-connect` CIDR alias (the Lightsail console's
// browser-based SSH client), so a satellite has no internet-facing sshd.
package aws

import "slices"

// anyIPv4 / anyIPv6 are the open-to-the-world CIDRs.
const (
	anyIPv4 = "0.0.0.0/0"
	anyIPv6 = "::/0"
)

// cidrAliasLightsailConnect is the only CIDR alias Lightsail supports: the
// address range of the console's browser SSH/RDP client.
const cidrAliasLightsailConnect = "lightsail-connect"

// PortRule is one public-port rule, in the adapter's own shape so callers
// need not know the Lightsail wire types.
type PortRule struct {
	Protocol string // "tcp", "udp", "icmp"
	FromPort int
	ToPort   int
	// Aliases restricts the rule to a Lightsail CIDR alias instead of a
	// CIDR range. A rule with an alias is NOT open to the internet.
	Aliases []string
}

// DefaultSatellitePorts is the satellite's public-port set: HTTP/HTTPS for
// the endpoint and ACME, the Kubernetes API and kubelet for the local
// cluster, flannel's VXLAN pair over UDP, and SSH limited to the Lightsail
// console client.
func DefaultSatellitePorts() []PortRule {
	return []PortRule{
		{Protocol: "tcp", FromPort: 80, ToPort: 80},
		{Protocol: "tcp", FromPort: 443, ToPort: 443},
		{Protocol: "tcp", FromPort: 6443, ToPort: 6443},
		{Protocol: "tcp", FromPort: 10250, ToPort: 10250},
		{Protocol: "udp", FromPort: 8475, ToPort: 8476},
		{Protocol: "tcp", FromPort: 22, ToPort: 22, Aliases: []string{cidrAliasLightsailConnect}},
	}
}

// WebPorts is the minimal set for a request that only asks for HTTP and
// HTTPS (plus console SSH, which is never a public exposure).
func WebPorts() []PortRule {
	return []PortRule{
		{Protocol: "tcp", FromPort: 80, ToPort: 80},
		{Protocol: "tcp", FromPort: 443, ToPort: 443},
		{Protocol: "tcp", FromPort: 22, ToPort: 22, Aliases: []string{cidrAliasLightsailConnect}},
	}
}

// wire renders a rule as a Lightsail portInfo. A rule with aliases carries
// no CIDRs at all, so it cannot accidentally be world-open.
func (r PortRule) wire() portRule {
	p := portRule{Protocol: r.Protocol, FromPort: r.FromPort, ToPort: r.ToPort}
	if len(r.Aliases) > 0 {
		p.CIDRListAliases = slices.Clone(r.Aliases)
		return p
	}
	p.CIDRs = []string{anyIPv4}
	if r.Protocol != "icmp" {
		p.IPv6CIDRs = []string{anyIPv6}
	}
	return p
}

func wirePorts(rules []PortRule) []portRule {
	out := make([]portRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.wire())
	}
	return out
}
