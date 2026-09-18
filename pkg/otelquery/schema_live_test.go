package otelquery

import "testing"

// The test whose absence let every trace verb ship broken.
//
// VictoriaTraces returns a row schema NO mock in this package used until it was caught against
// a live store: attributes are prefixed by origin, and event attributes carry a trailing index.
// The verbs queried and read the BARE names, so each returned a plausible "0 matches" — "I
// looked in a schema that does not exist," dressed as "nothing happened."
//
// These fixtures are the exact keys a live VictoriaTraces emitted for bashy's own spans. If the
// schema-aware resolver regresses to bare-name lookups, this fails.
func TestFieldResolvesTheRealStoreSchema(t *testing.T) {
	cases := []struct {
		name string
		row  map[string]any
		want string
		key  []string
	}{
		{"span attribute", map[string]any{"span_attr:cmd.exit_code": "2"}, "2", []string{"cmd.exit_code"}},
		{"resource attribute", map[string]any{"resource_attr:service.name": "bashy"}, "bashy", []string{"service.name"}},
		{"event attribute (indexed)", map[string]any{"event:event_attr:value.source:0": "GUESS-default-rate"}, "GUESS-default-rate", []string{"value.source"}},
		{"bare top-level field", map[string]any{"trace_id": "abc"}, "abc", []string{"trace_id"}},
		{"prefer bare over prefixed", map[string]any{"name": "exec ls", "span_attr:name": "x"}, "exec ls", []string{"name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := field(tc.row, tc.key...); got != tc.want {
				t.Errorf("field(%v) = %q, want %q — the store schema is not being resolved, so the "+
					"verb that reads this field returns empty against real data", tc.key, got, tc.want)
			}
		})
	}
}
