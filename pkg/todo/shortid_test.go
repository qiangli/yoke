package todo

import "testing"

// `it.ID[:8]` panicked the entire command on the first record with an empty id.
// The fault was one malformed file; the failure was every item on the list.
func TestShortID_SurvivesRecordsWithNoID(t *testing.T) {
	cases := map[string]string{
		"":                 "(no-id)",
		"abc":              "abc",
		"8be89e33":         "8be89e33",
		"8be89e33aaaabbbb": "8be89e33",
	}
	for in, want := range cases {
		if got := shortID(in); got != want {
			t.Errorf("shortID(%q) = %q, want %q", in, got, want)
		}
	}
}
