package provider_test

// The endpoint-reservation capability (S522.2 rework 3): the sslip.io
// spelling, and the fact that guarding an adapter does not hide the
// capability.

import (
	"context"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
)

func TestSslipEndpointUsesTheDashedSingleLabelForm(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The dashed form is one label under sslip.io, which is what a
		// wildcard certificate covers; the dotted form is not.
		{"52.12.110.80", "https://52-12-110-80.sslip.io"},
		{"10.0.0.1", "https://10-0-0-1.sslip.io"},
		{"  192.0.2.7  ", "https://192-0-2-7.sslip.io"},
		// An IPv4-mapped IPv6 literal is still an IPv4 address.
		{"::ffff:192.0.2.7", "https://192-0-2-7.sslip.io"},
		// Not addresses this can speak for.
		{"2001:db8::1", ""},
		{"host.example.com", ""},
		{"", ""},
		{"999.1.1.1", ""},
	} {
		if got := provider.SslipEndpoint(tc.in); got != tc.want {
			t.Errorf("SslipEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// reservingAdapter is a minimal adapter that reserves an address.
type reservingAdapter struct {
	manifest provider.Manifest
	calls    int
}

func (a *reservingAdapter) Manifest() provider.Manifest { return a.manifest }

func (a *reservingAdapter) Do(context.Context, provider.Request) (provider.Result, error) {
	return provider.Failure(provider.OpCreate, provider.ErrUnsupportedCapability, "fixture")
}

func (a *reservingAdapter) ReserveEndpoint(context.Context, provider.Spec) (provider.Reservation, error) {
	a.calls++
	return provider.Reservation{Name: "held", Address: "192.0.2.9", Endpoint: provider.SslipEndpoint("192.0.2.9")}, nil
}

// plainAdapter reserves nothing.
type plainAdapter struct{ manifest provider.Manifest }

func (a *plainAdapter) Manifest() provider.Manifest { return a.manifest }

func (a *plainAdapter) Do(context.Context, provider.Request) (provider.Result, error) {
	return provider.Failure(provider.OpCreate, provider.ErrUnsupportedCapability, "fixture")
}

// The dispatcher guards a built-in adapter, and aws.New guards it again,
// so a capability that did not survive wrapping would be silently lost at
// exactly the moment it matters.
func TestReserveEndpointSurvivesGuarding(t *testing.T) {
	m := provider.Manifest{
		Name: "fixture", Kind: provider.KindInfra, Form: provider.FormBuiltin,
		ProtocolVersion: provider.ProtocolVersion,
		Capabilities:    []provider.Capability{"create"},
	}
	inner := &reservingAdapter{manifest: m}
	wrapped := provider.Guard(provider.Guard(inner))

	got, err := provider.ReserveEndpoint(context.Background(), wrapped, provider.Spec{Name: "sat"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != "192.0.2.9" || got.Endpoint != "https://192-0-2-9.sslip.io" {
		t.Fatalf("reservation = %+v", got)
	}
	if inner.calls != 1 {
		t.Fatalf("inner adapter called %d time(s), want 1", inner.calls)
	}
}

// An adapter without the capability is not an error: there is simply no
// hint to send, and the provisioning sequence carries on.
func TestReserveEndpointOnAnAdapterWithoutTheCapability(t *testing.T) {
	m := provider.Manifest{
		Name: "plain", Kind: provider.KindInfra, Form: provider.FormBuiltin,
		ProtocolVersion: provider.ProtocolVersion,
		Capabilities:    []provider.Capability{"create"},
	}
	got, err := provider.ReserveEndpoint(context.Background(), provider.Guard(&plainAdapter{manifest: m}), provider.Spec{Name: "sat"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Held() {
		t.Fatalf("reservation = %+v, want nothing held", got)
	}
}
