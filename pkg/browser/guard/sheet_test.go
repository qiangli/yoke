package guard

import (
	"errors"
	"strings"
	"testing"
)

// fakeSheet is a sheet store: entries with a site, held-once ones consumed.
type fakeSheet struct {
	site map[string]string
	val  map[string]string
	once map[string]bool
}

func (f *fakeSheet) source() SheetSource {
	return SheetSource{
		Site: func(n string) string { return f.site[n] },
		Take: func(n string) (string, error) {
			v, ok := f.val[n]
			if !ok {
				return "", errors.New("no stored value")
			}
			if f.once[n] {
				delete(f.val, n)
			}
			return v, nil
		},
	}
}

func installSheet(t *testing.T, f *fakeSheet) {
	t.Helper()
	SetSheetSource(f.source())
	t.Cleanup(func() { SetSheetSource(SheetSource{}) })
}

// S529.9: a sheet entry fills ONE field by reference, on the site it was
// entered for; a held-once entry fills once; the value never comes back.
func TestFromSheetFillsByReferenceOnTheSameSiteOnly(t *testing.T) {
	askDir, home := isolate(t)
	f := &fakeSheet{
		site: map[string]string{"pw": "form.typeform.com", "elsewhere": "evil.example", "nosite": ""},
		val:  map[string]string{"pw": "hunter2-correct-horse", "elsewhere": "x-secret-1", "nosite": "x-secret-2", "otp": "493817"},
		once: map[string]bool{"otp": true},
	}
	f.site["otp"] = "form.typeform.com"
	installSheet(t, f)
	b := &fakeBackend{human: true}

	a := typeAction("")
	a.FromSheet = "pw"
	res := run(t, b, a)
	if res.OutcomeClass != ClassFilledByHuman || !res.Success || len(b.filled) != 1 || b.filled[0] != "hunter2-correct-horse" {
		t.Fatalf("same-site fill: %+v filled=%q", res, b.filled)
	}
	if !strings.Contains(res.Data, `"via":"sheet"`) || !strings.Contains(res.Data, `"verified":"match"`) {
		t.Errorf("result data: %s", res.Data)
	}
	assertNoLeak(t, "hunter2-correct-horse", res, askDir, home)
	assertNoHash(t, "hunter2-correct-horse", res)

	for _, name := range []string{"elsewhere", "nosite"} {
		x := typeAction("")
		x.FromSheet = name
		if res := run(t, b, x); res.OutcomeClass != ClassStopped || res.Reason != "from-sheet:other-site" || len(b.filled) != 1 {
			t.Errorf("%s: filled on the wrong site: %+v", name, res)
		}
	}

	o := typeAction("")
	o.FromSheet = "otp"
	if res := run(t, b, o); res.OutcomeClass != ClassFilledByHuman || len(b.filled) != 2 {
		t.Fatalf("held-once fill: %+v", res)
	}
	if res := run(t, b, o); res.OutcomeClass != ClassStopped || len(b.filled) != 2 {
		t.Fatalf("a held-once entry filled twice: %+v", res)
	}

	withText := a
	withText.Text = "hunter2-correct-horse"
	if res := run(t, b, withText); res.Success || len(b.filled) != 2 {
		t.Fatalf("--from-sheet with text went through: %+v", res)
	}
	if len(b.executed) != 0 {
		t.Fatalf("the agent's own action reached the page: %v", b.executed)
	}
}

func TestFromSheetWithoutAStoreStops(t *testing.T) {
	isolate(t)
	SetSheetSource(SheetSource{})
	a := typeAction("")
	a.FromSheet = "pw"
	b := &fakeBackend{human: true}
	if res := run(t, b, a); res.OutcomeClass != ClassStopped || len(b.filled) != 0 {
		t.Fatalf("filled without a sheet store: %+v", res)
	}
}
