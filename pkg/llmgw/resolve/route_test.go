package resolve

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func reqWithRouteHeader(v string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if v != "" {
		r.Header.Set(RouteHeader, v)
	}
	return r
}

// TestResolveRoutePolicy_Precedence walks the full precedence chain:
// per-request override → the principal's default → shipped default.
func TestResolveRoutePolicy_Precedence(t *testing.T) {
	cases := []struct {
		name             string
		header           string
		body             []byte
		principalDefault string
		want             string
	}{
		{
			name:             "request header beats the principal default",
			header:           RoutePrivacy,
			principalDefault: RouteCost,
			want:             RoutePrivacy,
		},
		{
			name:             "body route field used when no header",
			body:             []byte(`{"model":"m","route":"privacy"}`),
			principalDefault: RouteCost,
			want:             RoutePrivacy,
		},
		{
			name:             "header wins over body field",
			header:           RouteCost,
			body:             []byte(`{"route":"privacy"}`),
			principalDefault: RouteSmart,
			want:             RouteCost,
		},
		{
			name:             "invalid header falls through to the principal default",
			header:           "bogus-posture",
			principalDefault: RouteCost,
			want:             RouteCost,
		},
		{
			name:             "invalid body field falls through to the principal default",
			body:             []byte(`{"route":"bogus-posture"}`),
			principalDefault: RouteSmart,
			want:             RouteSmart,
		},
		{
			name:             "principal default when the request says nothing",
			principalDefault: RouteSmart,
			want:             RouteSmart,
		},
		{
			name:             "casing and padding are normalized at every level",
			principalDefault: "  SMART ",
			want:             RouteSmart,
		},
		{
			name: "shipped default when nothing is configured",
			want: DefaultRoutePolicy,
		},
		{
			name:             "a stale stored value falls through, it is not locked in",
			principalDefault: "garbage-account",
			want:             DefaultRoutePolicy,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveRoutePolicy(reqWithRouteHeader(tc.header), tc.body, tc.principalDefault)
			if got != tc.want {
				t.Fatalf("ResolveRoutePolicy = %q, want %q", got, tc.want)
			}
		})
	}
}

// A nil request is a non-HTTP caller (a job replay, an internal
// dispatch): body and defaults still decide, nothing panics.
func TestResolveRoutePolicy_NilRequest(t *testing.T) {
	if got := ResolveRoutePolicy(nil, []byte(`{"route":"cost"}`), ""); got != RouteCost {
		t.Fatalf("ResolveRoutePolicy(nil request) = %q, want %q", got, RouteCost)
	}
	if got := ResolveRoutePolicy(nil, nil, ""); got != DefaultRoutePolicy {
		t.Fatalf("ResolveRoutePolicy(nil request, no body) = %q, want %q", got, DefaultRoutePolicy)
	}
}

func TestNormalizeRoutePolicy(t *testing.T) {
	for in, want := range map[string]string{
		"local_first":   RouteLocalFirst,
		" PRIVACY ":     RoutePrivacy,
		"cost":          RouteCost,
		"Smart":         RouteSmart,
		"":              "",
		"local-first":   "",
		"bogus-posture": "",
	} {
		if got := NormalizeRoutePolicy(in); got != want {
			t.Errorf("NormalizeRoutePolicy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPeekRouteField(t *testing.T) {
	cases := []struct {
		body []byte
		want string
	}{
		{[]byte(`{"route":"cost"}`), "cost"},
		{[]byte(`{"model":"m","route":"  smart  "}`), "smart"},
		{[]byte(`{"model":"m"}`), ""},
		{[]byte(``), ""},
		{[]byte(`not json`), ""},
		{[]byte(`["array","not","object"]`), ""},
	}
	for _, tc := range cases {
		if got := PeekRouteField(tc.body); got != tc.want {
			t.Fatalf("PeekRouteField(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestRequestAllowsRemote(t *testing.T) {
	withHeader := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if v != "" {
			r.Header.Set(AllowRemoteHeader, v)
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		body []byte
		want bool
	}{
		{"header true", withHeader("true"), nil, true},
		{"header on beats a falsy body", withHeader("on"), []byte(`{"allow_cloud":false}`), true},
		{"header garbage is not an opt-in", withHeader("maybe"), nil, false},
		{"body flag", withHeader(""), []byte(`{"model":"m","allow_cloud":true}`), true},
		{"body flag false", withHeader(""), []byte(`{"allow_cloud":false}`), false},
		{"no signal at all", withHeader(""), []byte(`{"model":"m"}`), false},
		{"unparseable body", withHeader(""), []byte(`not json`), false},
		{"nil request, body opt-in", nil, []byte(`{"allow_cloud":true}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestAllowsRemote(tc.r, tc.body); got != tc.want {
				t.Fatalf("RequestAllowsRemote = %v, want %v", got, tc.want)
			}
		})
	}
}
