package src

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/qiangli/yoke/pkg/shape/appserver"
	"github.com/qiangli/yoke/pkg/shape/state"
)

// TidyPolicy controls which versions remain supported. A negative override
// takes the manifest value; absent manifest values default to 10 and 30 days.
type TidyPolicy struct {
	KeepLast, KeepDays int
	Now                time.Time
}

type TidyPlan struct {
	App                string         `json:"app"`
	Principal          string         `json:"principal"`
	Tip                string         `json:"tip,omitempty"`
	Base               string         `json:"base,omitempty"`
	KeepLast           int            `json:"keep_last"`
	KeepDays           int            `json:"keep_days"`
	Unit               string         `json:"unit,omitempty"`
	Kept               []VersionEntry `json:"kept,omitempty"`
	Squashed           []VersionEntry `json:"squashed,omitempty"`
	ContractProperties []string       `json:"contract_properties,omitempty"`
}

// PlanTidy only reads the selected principal's line and state. Retention is
// inclusive: any one policy rule keeps a version, as do active, named,
// adopted, offered, and review revisions.
func PlanTidy(home, app, principal string, policy TidyPolicy) (TidyPlan, error) {
	if home == "" {
		home = Home()
	}
	h, err := History(home, app, principal)
	if err != nil {
		return TidyPlan{}, err
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return TidyPlan{}, err
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return TidyPlan{}, err
	}
	p := TidyPlan{App: app, Principal: principal, Tip: h.Tip, Base: h.Base, KeepLast: 10, KeepDays: 30}
	active := h.Active
	if active == "" {
		active = h.Base
	}
	if active != "" {
		c, err := r.CommitObject(plumbing.NewHash(active))
		if err != nil {
			return p, fmt.Errorf("src: tidy active revision: %w", err)
		}
		tr, err := c.Tree()
		if err != nil {
			return p, err
		}
		if f, err := tr.File(appserver.File); err == nil {
			body, err := f.Contents()
			if err != nil {
				return p, err
			}
			m, err := appserver.Parse([]byte(body))
			if err != nil {
				return p, err
			}
			if m.Tidy != nil {
				if m.Tidy.KeepLast > 0 {
					p.KeepLast = m.Tidy.KeepLast
				}
				if m.Tidy.KeepDays > 0 {
					p.KeepDays = m.Tidy.KeepDays
				}
				p.Unit = m.Tidy.Unit
			}
		}
		unit := p.Unit
		if unit == "" {
			unit = "tidy"
		}
		if _, err := tr.File("api/" + unit + ".md"); err == nil {
			p.Unit = unit
		}
		if p.Unit != "" {
			if _, err := tr.File("api/" + p.Unit + ".md"); err != nil {
				return p, fmt.Errorf("src: tidy declared unit %q missing", p.Unit)
			}
		}
	}
	if policy.KeepLast >= 0 {
		p.KeepLast = policy.KeepLast
	}
	if policy.KeepDays >= 0 {
		p.KeepDays = policy.KeepDays
	}
	if p.KeepLast < 0 || p.KeepDays < 0 {
		return p, fmt.Errorf("src: tidy retention cannot be negative")
	}
	now := policy.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	protected := map[string]bool{h.Active: true}
	for _, n := range h.Named {
		protected[n.Revision] = true
	}
	for _, n := range h.Reviews {
		protected[n.Revision] = true
	}
	st, err := state.OpenAt(home, app, principal)
	if err != nil {
		return p, err
	}
	s, err := st.Read()
	if err != nil {
		return p, err
	}
	protected[s.LastKnownGood] = true
	for _, a := range s.Adopted {
		protected[a.Rev] = true
	}
	offers, err := ListOffers(home, app, principal)
	if err != nil {
		return p, err
	}
	for _, o := range offers {
		protected[o.Revision] = true
	}
	offered, err := OfferedRevisions(home, app, principal)
	if err != nil {
		return p, err
	}
	for _, rev := range offered {
		protected[rev] = true
	}
	for i, e := range h.Entries {
		keep := i == 0 || i < p.KeepLast || protected[e.Revision] || protected[e.Activation]
		if p.KeepDays > 0 && !e.When.Before(now.AddDate(0, 0, -p.KeepDays)) {
			keep = true
		}
		if keep {
			p.Kept = append(p.Kept, e)
		} else {
			p.Squashed = append(p.Squashed, e)
		}
	}
	// Contract candidates are properties that exist only in retired trees.
	// This is an offer to review an explicit B6 Contract step, never cleanup.
	retired := map[string]bool{}
	needed := map[string]bool{}
	propertySet := func(revisions []string, into map[string]bool) error {
		seen := map[string]bool{}
		for _, revision := range revisions {
			if revision == "" || seen[revision] {
				continue
			}
			seen[revision] = true
			c, err := r.CommitObject(plumbing.NewHash(revision))
			if err != nil {
				return err
			}
			tr, err := c.Tree()
			if err != nil {
				return err
			}
			f, err := tr.File(appserver.File)
			if err != nil {
				continue
			}
			body, err := f.Contents()
			if err != nil {
				return err
			}
			var m appserver.Manifest
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				return err
			}
			for _, prop := range m.Properties {
				into[prop] = true
			}
		}
		return nil
	}
	var removed []string
	for _, e := range p.Squashed {
		removed = append(removed, e.Revision)
	}
	if err := propertySet(removed, retired); err != nil {
		return p, err
	}
	var supported []string
	for _, e := range p.Kept {
		supported = append(supported, e.Revision)
	}
	supported = append(supported, h.Base, s.LastKnownGood, h.Active)
	if len(p.Squashed) > 0 {
		supported = append(supported, p.Squashed[0].Revision)
	} // new restorable base
	for _, n := range h.Named {
		supported = append(supported, n.Revision)
	}
	for _, n := range h.Reviews {
		supported = append(supported, n.Revision)
	}
	for _, a := range s.Adopted {
		supported = append(supported, a.Rev)
	}
	for _, o := range offers {
		supported = append(supported, o.Revision)
	}
	supported = append(supported, offered...)
	if err := propertySet(supported, needed); err != nil {
		return p, err
	}
	for prop := range retired {
		if !needed[prop] {
			p.ContractProperties = append(p.ContractProperties, prop)
		}
	}
	sort.Strings(p.ContractProperties)
	return p, nil
}

// ApplyTidy replaces the selected line with one synthetic base and copies
// every supported entry in order, retaining its content tree and trailers.
// The current line tip is checked again so a stale preview cannot overwrite
// a concurrent activation. The app's checkout and all other refs stay put.
func ApplyTidy(home string, p TidyPlan) error {
	if len(p.Squashed) == 0 {
		return nil
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, p.App)
	if err != nil {
		return err
	}
	tip, ok, err := refTarget(dir, VersionLineRef(p.Principal))
	if err != nil {
		return err
	}
	if !ok || tip != p.Tip {
		return fmt.Errorf("src: tidy line changed since preview")
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return err
	}
	// A base has no Principal trailer, so History stops at it. Its tree is
	// the newest retired version, preserving a useful end-of-life snapshot.
	baseEntry := p.Squashed[0]
	baseRev, err := r.CommitObject(plumbing.NewHash(baseEntry.Revision))
	if err != nil {
		return err
	}
	baseSHA, err := recordLineCommit(dir, plumbing.NewHash(p.Base), baseRev.TreeHash,
		"Tidy base for "+p.Principal+"\n\nTidy-Base: "+p.Tip+"\n", p.Principal, "tidy@bashy.local", time.Now().UTC())
	if err != nil {
		return err
	}
	parent := baseSHA
	for i := len(p.Kept) - 1; i >= 0; i-- {
		e := p.Kept[i]
		c, err := r.CommitObject(plumbing.NewHash(e.Activation))
		if err != nil {
			return err
		}
		msg := strings.TrimRight(c.Message, "\n") + "\nOriginal-Kind: " + e.Kind + "\nOriginal-Activation: " + e.Activation + "\n"
		parent, err = recordLineCommit(dir, plumbing.NewHash(parent), c.TreeHash, msg, c.Author.Name, c.Author.Email, c.Author.When)
		if err != nil {
			return err
		}
	}
	// Recheck before the only visible mutation.
	tip, ok, err = refTarget(dir, VersionLineRef(p.Principal))
	if err != nil {
		return err
	}
	if !ok || tip != p.Tip {
		return fmt.Errorf("src: tidy line changed during apply")
	}
	return setRefTarget(dir, VersionLineRef(p.Principal), parent)
}
