package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestEffectiveRequestPriority(t *testing.T) {
	for _, test := range []struct {
		name    string
		ceiling int
		header  string
		want    int
	}{
		{name: "no header", ceiling: 100, want: 100},
		{name: "downgrade", ceiling: 100, header: "200", want: 200},
		{name: "upgrade rejected", ceiling: 100, header: "50", want: 100},
		{name: "maximum", ceiling: 100, header: "9999", want: PriorityMax},
		{name: "negative", ceiling: 100, header: "-50", want: 100},
		{name: "invalid", ceiling: 100, header: "abc", want: 100},
		{name: "ceiling clamped low", ceiling: -1, want: PriorityMin},
		{name: "ceiling clamped high", ceiling: 2000, want: PriorityMax},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			request.Header.Set(PriorityHeader, test.header)
			if got := EffectiveRequestPriority(request, test.ceiling); got != test.want {
				t.Fatalf("priority = %d, want %d", got, test.want)
			}
		})
	}
}
