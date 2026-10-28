package guard

// Fill by reference. When the operator PROVIDES a guarded value, the guard
// records an admission: the opaque ask id bound to ONE field (FieldKey of the
// refused action's target) on one site, for a short while. The fill then
// names the ask id, never the value (wire.Action.FromAsk; on the CLI
// `browser type --from-ask <id> <target>`), and fillFromAsk:
//
//  1. claims the admission (an atomic rename — once per id), refusing a
//     different field;
//  2. resolves the value through the ValueSource (ask.TakeValue: once);
//  3. hands it to Backend.FillAdmitted, which bypasses the page-side refusal
//     that stops an agent's own text — and nothing else — and VERIFIES the
//     field afterwards, where the value already is;
//  4. returns guard:filled-by-human (or, when the page does not hold the
//     answer, guard:fill-mismatch) with the ask id, data.verified and the
//     two lengths — never the value, never a hash of it.
//
// The live hub accepts a raw guard_fill only for an id claimed for that same
// field (ConsumeClaimed), so a client that skips this package cannot use
// guard_fill to type around the field check.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

// AdmitTTL bounds how long a provided answer stays fillable (pkg/ask keeps
// the value 15 minutes).
var AdmitTTL = 15 * time.Minute

var askIDPattern = regexp.MustCompile(`^[0-9a-z]{32}$`)

type admission struct {
	ID      string    `json:"id"`
	Site    string    `json:"site,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Field   string    `json:"field"`
	Expires time.Time `json:"expires"`
}

// FieldKey identifies an action's target field: the selector, element id,
// visible-text match and scope it resolves by.
func FieldKey(a wire.Action) string {
	b, _ := json.Marshal([]any{a.Selector, a.ElementID, a.MatchText, a.Scope, a.IncludeHidden})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func admitDir() string {
	p := vaultCachePath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "browser-guard-admit")
}

func admitPath(id, suffix string) (string, error) {
	if !askIDPattern.MatchString(id) {
		return "", errors.New("guard: invalid ask id")
	}
	dir := admitDir()
	if dir == "" {
		return "", errors.New("guard: no cache directory")
	}
	return filepath.Join(dir, id+suffix), nil
}

// admit records that ask id may fill a's target, once.
func admit(id string, ref refusal, a wire.Action) error {
	p, err := admitPath(id, ".json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(admission{ID: id, Site: ref.Site, Reason: ref.Reason, Field: FieldKey(a), Expires: time.Now().Add(AdmitTTL)})
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	return f.Close()
}

// claim takes the admission for id if it was made for a's target. A wrong
// field leaves the admission in place for the right one.
func claim(id string, a wire.Action) (admission, error) {
	open, err := admitPath(id, ".json")
	if err != nil {
		return admission{}, err
	}
	claimed, _ := admitPath(id, ".claimed")
	if err := os.Rename(open, claimed); err != nil {
		return admission{}, errors.New("guard: ask id not admitted (or already used)")
	}
	adm, err := readAdmission(claimed)
	if err != nil || time.Now().After(adm.Expires) {
		os.Remove(claimed)
		return admission{}, errors.New("guard: admission expired")
	}
	if adm.Field != FieldKey(a) {
		_ = os.Rename(claimed, open)
		return admission{}, errors.New("guard: ask id was admitted for a different field")
	}
	return adm, nil
}

func readAdmission(p string) (admission, error) {
	var adm admission
	b, err := os.ReadFile(p)
	if err != nil {
		return adm, err
	}
	return adm, json.Unmarshal(b, &adm)
}

func release(id string) {
	if p, err := admitPath(id, ".claimed"); err == nil {
		os.Remove(p)
	}
}

// ConsumeClaimed is the live hub's check of a raw guard_fill: id must be
// mid-fill (claimed by fillFromAsk in this or another bashy process) for
// exactly a's target. It consumes the claim, so one claim admits one fill.
func ConsumeClaimed(id string, a wire.Action) error {
	p, err := admitPath(id, ".claimed")
	if err != nil {
		return err
	}
	adm, err := readAdmission(p)
	if err != nil {
		return errors.New("guard: guard_fill without an admitted operator answer")
	}
	os.Remove(p)
	if time.Now().After(adm.Expires) || adm.Field != FieldKey(a) {
		return errors.New("guard: guard_fill not admitted for this field")
	}
	return nil
}

// fillFromAsk fills a's target with the operator's answer named by
// a.FromAsk. The outcome is guard:filled-by-human, guard:fill-mismatch or
// guard:stopped.
func fillFromAsk(ctx context.Context, b Backend, a wire.Action) *wire.Result {
	if a.Type != wire.ActionType || a.Text != "" {
		return &wire.Result{Error: "guard: --from-ask fills a type by reference: give a target and no text"}
	}
	adm, err := claim(a.FromAsk, a)
	if err != nil {
		return outcome(ClassStopped, refusal{Reason: "from-ask:not-admitted"}, "not-admitted", "")
	}
	defer release(adm.ID)
	ref := refusal{Reason: adm.Reason, Site: adm.Site}
	_, values := seams()
	if values == nil {
		return outcome(ClassStopped, ref, "no-value-source", "")
	}
	v, err := values(adm.ID)
	if err != nil || len(v) == 0 {
		return outcome(ClassStopped, ref, "value-unavailable", "")
	}
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
	res := outcome(class, ref, "operator", adm.ID)
	return withVerify(res, ver)
}
