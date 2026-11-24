package privtest

import (
	"strings"
	"testing"
)

func TestEvalDACL(t *testing.T) {
	const me, other = "S-1-5-21-1-2-3-1001", "S-1-1-0"
	allow := func(s string) ace { return ace{typ: aceAccessAllowed, sid: s} }
	cases := []struct {
		name  string
		owner string
		null  bool
		aces  []ace
		want  string // "" = private; else substring of the error
	}{
		{"private", me, false, []ace{allow(me), allow(sidSystem), allow(sidAdmins)}, ""},
		{"system owns, me allowed", sidSystem, false, []ace{allow(me)}, ""},
		{"owner other", other, false, []ace{allow(me)}, "owner"},
		{"null dacl", me, true, nil, "NULL DACL"},
		{"empty dacl", me, false, nil, "no applicable allow"},
		{"everyone allowed", me, false, []ace{allow(me), allow(other)}, "S-1-1-0"},
		{"me missing", me, false, []ace{allow(sidSystem), allow(sidAdmins)}, "no applicable allow"},
		{"me only inherit-only", me, false, []ace{{typ: aceAccessAllowed, flags: aceInheritOnly, sid: me}}, "no applicable allow"},
		{"everyone inherit-only ignored", me, false, []ace{allow(me), {typ: aceAccessAllowed, flags: aceInheritOnly, sid: other}}, ""},
		{"deny ignored", me, false, []ace{{typ: aceAccessDenied}, {typ: aceDeniedObject}, {typ: aceDeniedCB}, {typ: aceDeniedCBObj}, allow(me)}, ""},
		{"deny does not stand in for me", me, false, []ace{{typ: aceAccessDenied, sid: me}}, "no applicable allow"},
		{"callback allow fails closed", me, false, []ace{allow(me), {typ: 0x09}}, "unhandled"},
		{"object allow fails closed", me, false, []ace{allow(me), {typ: 0x05}}, "unhandled"},
		{"compound allow fails closed", me, false, []ace{allow(me), {typ: 0x04}}, "unhandled"},
		{"callback object allow fails closed", me, false, []ace{allow(me), {typ: 0x0B}}, "unhandled"},
		{"unknown type fails closed", me, false, []ace{allow(me), {typ: 0x7f}}, "unhandled"},
	}
	for _, c := range cases {
		err := evalDACL(c.owner, c.null, c.aces, me)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}
