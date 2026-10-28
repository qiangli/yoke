// Static-IP-first provisioning (S522.2 rework 3).
//
// The first live zoo-aws create came up and never enrolled: the master
// renders the first-boot script with the satellite's endpoint baked in,
// and the only endpoint it could infer was DigitalOcean-shaped. The fix
// on this side is to stop deriving the endpoint from the host at all —
// the address is reserved BEFORE the host exists, so the bundle can be
// rendered with the endpoint the satellite will actually answer on, and
// the attachment cannot race first boot.
package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
)

// ReserveEndpoint allocates (or takes over) the "<name>-ip" static IP for
// an instance that does not exist yet and reports the sslip.io endpoint
// that address answers on. It implements provider.EndpointReserver, which
// the provisioning sequence calls ahead of the enrollment bundle.
//
// It is idempotent, because a provision is: an address that already
// exists is REUSED when it is free or already attached to this very
// instance (a replay), and an address attached to a DIFFERENT instance is
// refused rather than moved — taking an address off a running satellite
// would break the one host that is working.
//
// Allocating ahead of the instance is safe and free: a Lightsail static
// IP is regional, not tied to an instance, and is billed only while it is
// NOT attached to a running one. A reservation that is never used is
// therefore a small ongoing charge, which is why destroy releases it and
// why create attaches it rather than allocating a second one.
func (a *adapter) ReserveEndpoint(ctx context.Context, spec provider.Spec) (provider.Reservation, error) {
	if a.cfg.SkipStaticIP {
		// This adapter was opened without static IPs; there is no stable
		// address to promise, so there is no hint to send either.
		return provider.Reservation{}, nil
	}
	name := strings.TrimSpace(spec.Name)
	if !instanceNamePattern.MatchString(name) {
		return provider.Reservation{}, &provider.Error{Code: provider.ErrInvalidRequest,
			Message: fmt.Sprintf("aws: cannot reserve an address for instance name %q; it must match Lightsail's \\w[\\w\\-]*\\w", name)}
	}
	ipName := name + staticIPSuffix
	reused := true
	sip, err := a.getStaticIP(ctx, ipName)
	if err != nil {
		if normalize(err).Code != provider.ErrNotFound {
			return provider.Reservation{}, normalize(err)
		}
		reused = false
		if sip, err = a.allocateStaticIP(ctx, ipName); err != nil {
			return provider.Reservation{}, normalize(err)
		}
	}
	if sip.AttachedTo != "" && sip.AttachedTo != name {
		return provider.Reservation{}, &provider.Error{Code: provider.ErrNotOwned, Message: fmt.Sprintf(
			"aws: static ip %s is attached to %s, not %s; refusing to move it", ipName, sip.AttachedTo, name)}
	}
	if sip.IPAddress == "" {
		return provider.Reservation{}, &provider.Error{Code: provider.ErrProvider, Message: fmt.Sprintf(
			"aws: static ip %s reports no address", ipName)}
	}
	return provider.Reservation{
		Name:     ipName,
		Address:  sip.IPAddress,
		Endpoint: provider.SslipEndpoint(sip.IPAddress),
		Reused:   reused,
	}, nil
}
