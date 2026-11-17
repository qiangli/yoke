package guard

// Fill by reference from a copy sheet (Sprint 529, S529.9). When the guard
// would need several values from the human (a login's user, password and
// one-time code), the agent lists them as named secrets on ONE served copy
// sheet instead of raising a dialog per field; the human enters them there
// (S529.8: Remember, or held once). The fill then names the sheet entry —
// wire.Action.FromSheet, on the CLI `browser type --from-sheet NAME <target>`
// — and fillFromSheet:
//
//  1. refuses unless the entry was made for THIS page's site (the hub
//     records the driven tab's site when it serves the sheet), so a value
//     cannot be carried to another site;
//  2. records and claims an admission for exactly this field (admit.go), so
//     the live hub's guard_fill check holds as for --from-ask;
//  3. takes the value through the SheetSource (a held-once entry is consumed;
//     a remembered one stays for the next fill on the same site);
//  4. fills and verifies through Backend.FillAdmitted and returns
//     guard:filled-by-human or guard:fill-mismatch — never the value.
//
// The host installs the SheetSource (cmds/browser over pkg/browser/live's
// sheet store); this package does not import live.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// SheetSource resolves copy-sheet entries. Site reports the site an entry
// was made for ("" when none); Take returns its value (consuming a held-once
// entry).
type SheetSource struct {
	Site func(name string) string
	Take func(name string) (string, error)
}

var sheetSource SheetSource

// SetSheetSource installs the host's sheet store (zero value removes it).
func SetSheetSource(s SheetSource) {
	humanMu.Lock()
	sheetSource = s
	humanMu.Unlock()
}

func sheetSeam() SheetSource {
	humanMu.RLock()
	defer humanMu.RUnlock()
	return sheetSource
}

// fillFromSheet fills a's target with the sheet entry a.FromSheet names.
func fillFromSheet(ctx context.Context, b Backend, a wire.Action) *wire.Result {
	if a.Type != wire.ActionType || a.Text != "" || a.FromAsk != "" {
		return &wire.Result{Error: "guard: --from-sheet fills a type by reference: give a target and no text"}
	}
	src := sheetSeam()
	if src.Site == nil || src.Take == nil {
		return outcome(ClassStopped, refusal{Reason: "from-sheet:no-sheet-store"}, "no-value-source", "")
	}
	site := b.Site(ctx)
	entrySite := src.Site(a.FromSheet)
	ref := refusal{Reason: "from-sheet", Site: site}
	if site == "" || entrySite == "" || !strings.EqualFold(site, entrySite) {
		return outcome(ClassStopped, refusal{Reason: "from-sheet:other-site", Site: site}, "other-site", "")
	}
	id, err := sheetAdmitID()
	if err != nil {
		return outcome(ClassStopped, ref, "no-admission", "")
	}
	if err := admit(id, ref, a); err != nil {
		return outcome(ClassStopped, ref, "no-admission", "")
	}
	adm, err := claim(id, a)
	if err != nil {
		return outcome(ClassStopped, ref, "no-admission", "")
	}
	defer release(adm.ID)
	sv, err := src.Take(a.FromSheet)
	if err != nil || sv == "" {
		return outcome(ClassStopped, ref, "value-unavailable", "")
	}
	v := []byte(sv)
	defer clear(v)
	ver, err := b.FillAdmitted(ctx, a, adm.ID, v)
	if err != nil {
		res := outcome(ClassStopped, ref, "fill-failed", "")
		var ff FillFailure
		if errors.As(err, &ff) && causePattern.MatchString(ff.Cause) {
			res = withFields(res, map[string]any{"cause": ff.Cause})
		}
		return res
	}
	class := ClassFilledByHuman
	if ver.Verified != VerifiedMatch {
		ver.Verified, class = VerifiedMismatch, ClassFillMismatch
	}
	res := outcome(class, ref, "sheet", adm.ID)
	return withVerify(res, ver)
}

// sheetAdmitID is a fresh admission id in the ask-id shape (32 [0-9a-z]).
func sheetAdmitID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
