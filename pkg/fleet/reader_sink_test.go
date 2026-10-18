package fleet

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestSinkEscape(t *testing.T) {
	for _, c := range [][]string{{"--full"}, {"--view=canonical"}, {"--view", "canonical"}} {
		rest, canon := SinkEscape(append([]string{"a"}, c...))
		if !canon || !reflect.DeepEqual(rest, []string{"a"}) {
			t.Errorf("%v: rest=%v canon=%v", c, rest, canon)
		}
	}
	rest, canon := SinkEscape([]string{"--view", "json", "x"})
	if canon || len(rest) != 3 {
		t.Errorf("json view must not escape: %v %v", rest, canon)
	}
}

func TestApplyReaderSink(t *testing.T) {
	data := []byte(strings.Repeat("0123456789\n", 10))
	in := SinkInput{
		Prefs:   ReaderPrefs{BudgetBytes: 35},
		Sources: ReaderPrefsSources{BudgetBytes: SourceDefault},
		Recover: "bashy fleet list",
	}
	r := ApplyReaderSink(data, in)
	if !r.Truncated || r.Kept != 33 || r.Total != 110 {
		t.Fatalf("%+v", r)
	}
	if !bytes.HasPrefix(r.Output, data[:33]) ||
		!strings.Contains(string(r.Output), "bashy fleet list --full") ||
		!strings.Contains(string(r.Output), "budget_bytes=35 from "+SourceDefault) {
		t.Errorf("bad output %q", r.Output)
	}

	in.Canonical = true
	if r := ApplyReaderSink(data, in); r.Truncated || !bytes.Equal(r.Output, data) {
		t.Error("escape must pass canonical bytes")
	}
	in.Canonical = false
	in.Prefs.View = ViewCanonical
	if r := ApplyReaderSink(data, in); r.Truncated {
		t.Error("view=canonical must pass through")
	}
	in.Prefs = ReaderPrefs{BudgetBytes: 1000}
	if r := ApplyReaderSink(data, in); r.Truncated || !bytes.Equal(r.Output, data) {
		t.Error("under budget must be untouched")
	}
}
