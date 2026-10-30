package live

import (
	"testing"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// TestEffectsParamsReachTheExtension guards the silent-drop class for
// the new and extended knobs: a flag the CLI accepts but the transport
// discards is exactly how `screenshot <path>` came to exit 0 having
// done nothing.
func TestEffectsParamsReachTheExtension(t *testing.T) {
	method, params, err := actionToParams(wire.Action{Type: wire.ActionText, Selector: "#bio"})
	if err != nil {
		t.Fatal(err)
	}
	if method != "text" || params["selector"] != "#bio" {
		t.Fatalf("text params = %q %#v", method, params)
	}
	method, params, err = actionToParams(wire.Action{Type: wire.ActionScroll, Direction: "down", Amount: 500, Selector: "#panel"})
	if err != nil {
		t.Fatal(err)
	}
	if method != "scroll" || params["selector"] != "#panel" || params["amount"] != 500 {
		t.Fatalf("addressed scroll params = %q %#v", method, params)
	}
	method, params, err = actionToParams(wire.Action{Type: wire.ActionScroll, Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if method != "scroll" || params["direction"] != "down" {
		t.Fatalf("window scroll params = %q %#v", method, params)
	}
}

// TestExtensionVersionCarriesText: the text method needs extension
// 0.10.0+ (it is advertised, not defaulted). The floor stays where the
// stale-extension gate put it; this pins the new capability's arrival.
func TestExtensionVersionCarriesText(t *testing.T) {
	if versionLess(EmbeddedVersion(), "0.10.0") {
		t.Fatalf("embedded extension v%s predates the text method (0.10.0)", EmbeddedVersion())
	}
}
