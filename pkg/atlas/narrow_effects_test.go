package atlas

import (
	"reflect"
	"testing"
)

// A read-only subcommand of a mixed verb keeps only its read-side effects;
// anything not listed keeps the verb's worst case, and nothing is added.
func TestNarrowEffects(t *testing.T) {
	shape, ok := Lookup("shape")
	if !ok {
		t.Fatal("shape is not in the atlas")
	}
	cases := []struct {
		argv []string
		base []string
		want []string
	}{
		{[]string{"shape", "show", "app", "1"}, shape.Effects, []string{EffNet, EffRead}},
		{[]string{"shape", "diff", "app", "1"}, shape.Effects, []string{EffNet, EffRead}},
		{[]string{"shape", "approve", "app", "1"}, shape.Effects, shape.Effects},
		{[]string{"shape", "verify", "app", "1"}, shape.Effects, shape.Effects},
		{[]string{"shape"}, shape.Effects, shape.Effects},
		{[]string{"desktop", "get"}, []string{EffNet, EffRead, EffWrite}, []string{EffNet, EffRead}},
		{[]string{"desktop", "set", "theme", "light"}, []string{EffNet, EffRead, EffWrite}, []string{EffNet, EffRead, EffWrite}},
		{[]string{"todo", "list"}, []string{EffWrite}, []string{EffRead}},
		{[]string{"grep", "show"}, []string{EffRead}, []string{EffRead}},
	}
	for _, c := range cases {
		if got := NarrowEffects(c.argv, c.base); !reflect.DeepEqual(got, c.want) {
			t.Errorf("NarrowEffects(%v, %v) = %v, want %v", c.argv, c.base, got, c.want)
		}
	}
}
