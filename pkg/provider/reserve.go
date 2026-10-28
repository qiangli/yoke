// Endpoint reservation (S522.2 rework 3).
//
// A satellite's public endpoint has to be known BEFORE the host exists.
// The master renders the first-boot script with the endpoint baked into
// it, and a script that discovers its own address from cloud metadata
// works only on the one cloud whose metadata format it knows — which is
// exactly how the first zoo-aws create came up without ever enrolling.
//
// So a provider that can hold a stable address reserves it FIRST, the
// reserved address becomes the enrollment bundle's endpoint hint, and the
// host is created afterwards and attached to the address it was already
// told about. Nothing then depends on metadata, and nothing races the
// attachment against first boot.
//
// Reservation is an OPTIONAL capability, deliberately kept OFF the
// Adapter interface: a provider with no stable addresses stays exactly as
// it was and the provisioning sequence simply has no hint to send, rather
// than every adapter having to grow a method that answers "not me".
package provider

import (
	"context"
	"net"
	"strings"
)

// Reservation is a stable public address held for a host that does not
// exist yet. The zero value means nothing was reserved.
type Reservation struct {
	// Name is the provider's own name for the reservation (e.g. a
	// Lightsail static IP name), so a later destroy can find it.
	Name string `json:"name,omitempty"`
	// Address is the reserved IP.
	Address string `json:"address,omitempty"`
	// Endpoint is the public endpoint hint that address answers on, or
	// "" when the provider has no hint to offer for it.
	Endpoint string `json:"endpoint,omitempty"`
	// Reused is true when the reservation already existed and was taken
	// over rather than allocated. A replayed provision must not allocate
	// a second address.
	Reused bool `json:"reused,omitempty"`
}

// Held reports whether r holds an address.
func (r Reservation) Held() bool { return r.Address != "" }

// EndpointReserver is the optional capability of reserving a host's
// stable public address before the host is created. An adapter that
// implements it is asked once, ahead of enrollment.
type EndpointReserver interface {
	ReserveEndpoint(ctx context.Context, spec Spec) (Reservation, error)
}

// ReserveEndpoint reserves a stable address for spec through a, or
// returns the zero Reservation when a reserves nothing — an adapter
// without the capability is not an error, it just has no hint.
func ReserveEndpoint(ctx context.Context, a Adapter, spec Spec) (Reservation, error) {
	r, ok := a.(EndpointReserver)
	if !ok {
		return Reservation{}, nil
	}
	return r.ReserveEndpoint(ctx, spec)
}

// SslipEndpoint is the https endpoint an IPv4 address answers on through
// sslip.io, the wildcard DNS service that resolves an address written
// into its own domain straight back to that address:
// 52.12.110.80 becomes https://52-12-110-80.sslip.io.
//
// The DASHED spelling is the one used, not the dotted one: a dotted
// address is several DNS labels deep under sslip.io and a wildcard
// certificate covers exactly one label, so only the dashed form gets
// built-in TLS.
//
// It returns "" for anything that is not an IPv4 address: an IPv6 address
// has no dashed form here, and a host name needs no hint.
func SslipEndpoint(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil || parsed.To4() == nil {
		return ""
	}
	return "https://" + strings.ReplaceAll(parsed.To4().String(), ".", "-") + ".sslip.io"
}
