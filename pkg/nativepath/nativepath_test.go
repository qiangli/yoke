package nativepath

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"/c/bashy:/c/Windows/System32": `C:\bashy;C:\Windows\System32`,
		`C:\a;C:\b`:                    `C:\a;C:\b`,
		"":                             "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q)=%q want %q", in, got, want)
		}
	}
}
