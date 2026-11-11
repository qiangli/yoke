package cloudboxfake

import (
	"net/http"
	"sort"
	"strings"
)

func validSlug(s, code string) *apiError {
	if s == "public" || s == "owner" || len(s) < 1 || len(s) > 32 {
		return errf(400, code, "invalid name %q", s)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return errf(400, code, "invalid name %q", s)
		}
	}
	return nil
}
func (f *Fake) shareOwner(r *http.Request, host string) (*token, *apiError) {
	t, e := f.auth(r, ScopeHostsWrite)
	if e != nil {
		return nil, e
	}
	h := f.hosts[host]
	if h == nil || h.OwnerEmail != t.email {
		return nil, errf(403, "not_owner", "host is not owned by caller")
	}
	return t, nil
}
func (f *Fake) listHostShares(w http.ResponseWriter, r *http.Request, host string) *apiError {
	t, e := f.shareOwner(r, host)
	if e != nil {
		return e
	}
	out := []Share{}
	for _, s := range f.shares {
		if s.owner == t.email && s.Host == host {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"shares": out})
	return nil
}
func (f *Fake) addHostShare(w http.ResponseWriter, r *http.Request, host string) *apiError {
	t, e := f.shareOwner(r, host)
	if e != nil {
		return e
	}
	var b struct {
		App    string `json:"app"`
		Email  string `json:"email"`
		Circle string `json:"circle"`
	}
	if e := decode(r, &b); e != nil {
		return e
	}
	if validSlug(b.App, "invalid_share") != nil || (b.Email == "") == (b.Circle == "") {
		return errf(400, "invalid_share", "app and exactly one audience required")
	}
	if b.Circle != "" && validSlug(b.Circle, "invalid_circle") != nil {
		return errf(400, "invalid_circle", "invalid circle")
	}
	s := &Share{ID: f.nextID("shr"), Host: host, App: b.App, Email: strings.ToLower(b.Email), Circle: b.Circle, CreatedAt: f.now(), owner: t.email}
	f.shares[s.ID] = s
	writeJSON(w, 201, map[string]any{"share": s})
	return nil
}
func (f *Fake) deleteHostShare(w http.ResponseWriter, r *http.Request, host, id string) *apiError {
	t, e := f.shareOwner(r, host)
	if e != nil {
		return e
	}
	if s := f.shares[id]; s != nil && s.owner == t.email && s.Host == host {
		delete(f.shares, id)
	}
	w.WriteHeader(204)
	return nil
}
func (f *Fake) ownerSuite(r *http.Request, name string) (*token, *Suite, *apiError) {
	if e := validSlug(name, "invalid_suite"); e != nil {
		return nil, nil, e
	}
	t, e := f.auth(r, ScopeHostsWrite)
	if e != nil {
		return nil, nil, e
	}
	s := f.suites[t.email+"/"+name]
	return t, s, nil
}
func (f *Fake) listSuites(w http.ResponseWriter, r *http.Request) *apiError {
	t, e := f.auth(r, ScopeHostsWrite)
	if e != nil {
		return e
	}
	out := []Suite{}
	for _, s := range f.suites {
		if s.owner == t.email {
			v := *s
			v.Apps = append([]SuiteApp{}, s.Apps...)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"suites": out})
	return nil
}
func (f *Fake) showSuite(w http.ResponseWriter, r *http.Request, name string) *apiError {
	_, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	if s == nil {
		return errf(404, "not_found", "suite not found")
	}
	writeJSON(w, 200, map[string]any{"suite": s})
	return nil
}
func (f *Fake) suiteApps(r *http.Request, owner string) ([]SuiteApp, *apiError) {
	var b struct {
		Apps []SuiteApp `json:"apps"`
	}
	if e := decode(r, &b); e != nil {
		return nil, e
	}
	if b.Apps == nil {
		return nil, errf(400, "invalid_suite", "apps must be an array")
	}
	out := []SuiteApp{}
	seen := map[string]bool{}
	for _, a := range b.Apps {
		if validSlug(a.App, "invalid_suite") != nil {
			return nil, errf(400, "invalid_suite", "invalid app")
		}
		h := f.hosts[a.Host]
		if h == nil || h.OwnerEmail != owner {
			return nil, errf(403, "not_owner", "host is not owned by caller")
		}
		k := a.Host + "\x00" + a.App
		if !seen[k] {
			out = append(out, a)
			seen[k] = true
		}
	}
	return out, nil
}
func (f *Fake) putSuite(w http.ResponseWriter, r *http.Request, name string) *apiError {
	t, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	apps, e := f.suiteApps(r, t.email)
	if e != nil {
		return e
	}
	now := f.now()
	if s == nil {
		s = &Suite{Name: name, CreatedAt: now, owner: t.email}
	}
	s.Apps = apps
	s.UpdatedAt = now
	f.suites[t.email+"/"+name] = s
	writeJSON(w, 200, map[string]any{"suite": s})
	return nil
}
func (f *Fake) addSuiteApp(w http.ResponseWriter, r *http.Request, name string) *apiError {
	t, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	if s == nil {
		return errf(404, "not_found", "suite not found")
	}
	var a SuiteApp
	if e := decode(r, &a); e != nil {
		return e
	}
	h := f.hosts[a.Host]
	if h == nil || h.OwnerEmail != t.email {
		return errf(403, "not_owner", "host is not owned by caller")
	}
	if validSlug(a.App, "invalid_suite") != nil {
		return errf(400, "invalid_suite", "invalid app")
	}
	for _, x := range s.Apps {
		if x == a {
			writeJSON(w, 200, map[string]any{"suite": s})
			return nil
		}
	}
	s.Apps = append(s.Apps, a)
	s.UpdatedAt = f.now()
	writeJSON(w, 200, map[string]any{"suite": s})
	return nil
}
func (f *Fake) removeSuiteApp(w http.ResponseWriter, r *http.Request, name, host, app string) *apiError {
	_, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	if s == nil {
		return errf(404, "not_found", "suite not found")
	}
	out := s.Apps[:0]
	for _, a := range s.Apps {
		if a.Host != host || a.App != app {
			out = append(out, a)
		}
	}
	s.Apps = append([]SuiteApp{}, out...)
	s.UpdatedAt = f.now()
	w.WriteHeader(204)
	return nil
}
func (f *Fake) deleteSuite(w http.ResponseWriter, r *http.Request, name string) *apiError {
	t, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	if s != nil {
		delete(f.suites, t.email+"/"+name)
		for id, g := range f.suiteShares {
			if g.owner == t.email && g.Suite == name {
				delete(f.suiteShares, id)
			}
		}
	}
	w.WriteHeader(204)
	return nil
}
func (f *Fake) listSuiteShares(w http.ResponseWriter, r *http.Request, name string) *apiError {
	t, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	if s == nil {
		return errf(404, "not_found", "suite not found")
	}
	out := []SuiteShare{}
	for _, g := range f.suiteShares {
		if g.owner == t.email && g.Suite == name {
			out = append(out, *g)
		}
	}
	writeJSON(w, 200, map[string]any{"shares": out})
	return nil
}
func (f *Fake) addSuiteShare(w http.ResponseWriter, r *http.Request, name string) *apiError {
	t, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	if s == nil {
		return errf(404, "not_found", "suite not found")
	}
	var b struct {
		Email  string `json:"email"`
		Circle string `json:"circle"`
	}
	if e := decode(r, &b); e != nil {
		return e
	}
	if (b.Email == "") == (b.Circle == "") {
		return errf(400, "invalid_share", "exactly one audience required")
	}
	if b.Circle != "" && validSlug(b.Circle, "invalid_circle") != nil {
		return errf(400, "invalid_circle", "invalid circle")
	}
	g := &SuiteShare{ID: f.nextID("sgr"), Suite: name, Email: strings.ToLower(b.Email), Circle: b.Circle, CreatedAt: f.now(), owner: t.email}
	f.suiteShares[g.ID] = g
	writeJSON(w, 201, map[string]any{"share": g})
	return nil
}
func (f *Fake) deleteSuiteShare(w http.ResponseWriter, r *http.Request, name, id string) *apiError {
	t, s, e := f.ownerSuite(r, name)
	if e != nil {
		return e
	}
	if s == nil {
		return errf(404, "not_found", "suite not found")
	}
	if g := f.suiteShares[id]; g != nil && g.owner == t.email && g.Suite == name {
		delete(f.suiteShares, id)
	}
	w.WriteHeader(204)
	return nil
}

// CanReach evaluates all grants against live owner membership and suite state.
func (f *Fake) CanReach(email, host, app string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[host]
	if h == nil {
		return false
	}
	email = strings.ToLower(email)
	for _, s := range f.shares {
		if s.Host == host && s.App == app && s.owner == h.OwnerEmail && (s.Email == email || s.Circle != "" && f.inCircle(h.OwnerEmail, email, s.Circle)) {
			return true
		}
	}
	for _, g := range f.suiteShares {
		if g.owner != h.OwnerEmail || !(g.Email == email || g.Circle != "" && f.inCircle(h.OwnerEmail, email, g.Circle)) {
			continue
		}
		s := f.suites[g.owner+"/"+g.Suite]
		if s == nil {
			continue
		}
		for _, a := range s.Apps {
			if a.Host == host && a.App == app {
				return true
			}
		}
	}
	return false
}
func (f *Fake) inCircle(owner, email, circle string) bool {
	for _, m := range f.members {
		if m.owner == owner && strings.EqualFold(m.Email, email) && m.Circle == circle && m.Status == "active" && f.mutual(owner, email) {
			return true
		}
	}
	return false
}
