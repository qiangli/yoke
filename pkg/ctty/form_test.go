package ctty

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestFormPositiveEvidence(t *testing.T) {
	choices := []string{"Enter it here", "I'll type it in the page myself", "Skip this question", "Stop"}
	if _, err := parseFormAnswer([]byte(`{"choice":"","value":"x"}`), choices); !errors.Is(err, ErrDismissed) {
		t.Fatal(err)
	}
	if _, err := parseFormAnswer([]byte(`{"choice":"unknown","value":"x"}`), choices); !errors.Is(err, ErrDismissed) {
		t.Fatal(err)
	}
	got, err := parseFormAnswer([]byte(`{"choice":"Stop","value":""}`), choices)
	if err != nil || got.Choice != "Stop" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestNativeFormScriptContract(t *testing.T) {
	for _, want := range []string{"NSAlert", "NSTextField", "NSSecureTextField", "Show", "activateIgnoringOtherApps", "JSON.stringify", "ObjC.registerSubclass"} {
		if !strings.Contains(macFormSrc, want) {
			t.Errorf("mac form missing %s", want)
		}
	}
	for _, want := range []string{"System.Windows.Forms", "UseSystemPasswordChar", "Show", "ConvertTo-Json", "TopMost"} {
		if !strings.Contains(windowsFormSrc, want) {
			t.Errorf("Windows form missing %s", want)
		}
	}
}

func TestNativeFormGolden(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"mac_form.js", macFormSrc}, {"windows_form.ps1", windowsFormSrc},
	} {
		want, err := os.ReadFile("testdata/" + tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if string(want) != tc.source {
			t.Errorf("%s differs from reviewed script golden", tc.name)
		}
	}
}
