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
	for _, want := range []string{"NSAlert", "NSTextField", "NSSecureTextField", "Show", "activateIgnoringOtherApps", "JSON.stringify", "ObjC.registerSubclass", "NSPasteboard", "changeCount", "clearContents", "Clipboard history", "setMainMenu", "NSModalPanelRunLoopMode", "NSRunLoopCommonModes", "NSEventModifierFlagControl", "field.setMenu(contextMenu)", "editItem(editMenu,'Paste'"} {
		if !strings.Contains(macFormSrc, want) {
			t.Errorf("mac form missing %s", want)
		}
	}
	for _, want := range []string{"System.Windows.Forms", "UseSystemPasswordChar", "Show", "ConvertTo-Json", "TopMost", "Clipboard-Reason", "Clipboard]::Clear", "System.Windows.Forms.Timer", "$timer.Interval=350", "$timer.Add_Tick({ Refresh-Clipboard })"} {
		if !strings.Contains(windowsFormSrc, want) {
			t.Errorf("Windows form missing %s", want)
		}
	}
}

func TestRevealToggleStaysInForm(t *testing.T) {
	start := strings.Index(macFormSrc, "name:'BashyReveal'")
	end := strings.Index(macFormSrc, "var reveal=$.BashyReveal.alloc.init")
	if start < 0 || end <= start {
		t.Fatal("macOS form has no reveal action")
	}
	action := macFormSrc[start:end]
	for _, want := range []string{
		"from.currentEditor", "editor.selectedRange", "to.setStringValue(from.stringValue)",
		"sender.window.makeFirstResponder(to)", "to.currentEditor.setSelectedRange(selection)",
	} {
		if !strings.Contains(action, want) {
			t.Errorf("macOS reveal action missing %q", want)
		}
	}
	for _, forbidden := range []string{"becomeFirstResponder()", "runModal", "result.choice", "stopModal"} {
		if strings.Contains(action, forbidden) {
			t.Errorf("macOS reveal action unexpectedly contains %q", forbidden)
		}
	}
	if !strings.Contains(macFormSrc[end:], "show.setTarget(reveal); show.setAction('toggle:'); box.addSubview(show)") ||
		strings.Contains(macFormSrc, "alert.addButtonWithTitle('Show')") {
		t.Fatal("Show must be an accessory-view checkbox with its own action")
	}
	if !strings.Contains(windowsFormSrc, "$show.Add_CheckedChanged({ $t.UseSystemPasswordChar = -not $show.Checked })") ||
		!strings.Contains(windowsFormSrc, "$show.Visible=($Kind -eq 'secret')") {
		t.Fatal("Windows Show must toggle masking in place for secret fields")
	}
}

func TestClipboardPreviewChangeCount(t *testing.T) {
	var state clipboardPreview
	value := ""
	reads := 0
	read := func() (string, error) { reads++; return value, nil }
	if !state.update(10, "ssn", read) || state.enabled || state.reason != "clipboard is empty" {
		t.Fatalf("empty preview: %+v", state)
	}
	value = "hello"
	if state.update(10, "ssn", read) || reads != 1 {
		t.Fatalf("unchanged count re-read clipboard: %+v, reads=%d", state, reads)
	}
	if !state.update(11, "ssn", read) || state.enabled || state.reason == "" {
		t.Fatalf("wrong-kind preview: %+v", state)
	}
	value = "078-05-1120"
	if !state.update(12, "ssn", read) || !state.enabled || state.reason != "" {
		t.Fatalf("valid preview: %+v", state)
	}
	value = "newer copy"
	if !state.update(13, "ssn", read) || state.enabled {
		t.Fatalf("changed-before-click preview: %+v", state)
	}
}

func TestClipboardEligibilityAndConditionalClear(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"not-an-ssn", false}, {"123-45-6789", true},
	} {
		if got := clipboardEligible("ssn", tc.value); got != tc.want {
			t.Fatalf("value shape got %v, want %v", got, tc.want)
		}
	}
	cleared := false
	clearIfSame("123-45-6789", func() (string, error) { return "123-45-6789", nil }, func() { cleared = true })
	if !cleared {
		t.Fatal("unchanged clipboard was not cleared")
	}
	cleared = false
	clearIfSame("123-45-6789", func() (string, error) { return "newer copy", nil }, func() { cleared = true })
	if cleared {
		t.Fatal("newer clipboard was cleared")
	}
	if clipboardEligible("ssn", "newer copy") {
		t.Fatal("changed clipboard was not revalidated")
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
