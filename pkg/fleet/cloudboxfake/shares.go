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
			v := *s
			if v.Circles == nil {
				v.Circles = []string{}
			}
			if v.Groups == nil {
				v.Groups = []string{}
			}
			if v.Emails == nil {
				v.Emails = []string{}
			}
			out = append(out, v)
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
		App     string   `json:"app"`
		Circles []string `json:"circles"`
		Groups  []string `json:"groups"`
		Emails  []string `json:"emails"`
		Circle  string   `json:"circle"`
		Email   string   `json:"email"`
	}
	if e := decode(r, &b); e != nil {
		return e
	}
	if validSlug(b.App, "invalid_share") != nil {
		return errf(400, "invalid_share", "invalid app")
	}
	if b.Circle != "" && len(b.Circles) == 0 {
		b.Circles = []string{b.Circle}
	}
	if b.Email != "" && len(b.Emails) == 0 {
		b.Emails = []string{strings.ToLower(b.Email)}
	}
	if b.Circles == nil && b.Groups == nil && b.Emails == nil && b.Circle == "" && b.Email == "" {
		return errf(400, "invalid_share", "app and at least one audience rule required")
	}
	if b.Circle != "" && b.Email != "" {
		return errf(400, "invalid_share", "app and exactly one audience required")
	}
	for _, c := range b.Circles {
		if validSlug(c, "invalid_circle") != nil {
			return errf(400, "invalid_circle", "invalid circle %q", c)
		}
	}
	for _, g := range b.Groups {
		if validSlug(g, "invalid_group") != nil {
			return errf(400, "invalid_group", "invalid group %q", g)
		}
	}
	if b.Circles == nil {
		b.Circles = []string{}
	}
	if b.Groups == nil {
		b.Groups = []string{}
	}
	if b.Emails == nil {
		b.Emails = []string{}
	} else {
		for i, em := range b.Emails {
			b.Emails[i] = strings.ToLower(em)
		}
	}
	s := &Share{
		ID:      f.nextID("shr"),
		Host:    host,
		App:     b.App,
		Circles: b.Circles,
		Groups:  b.Groups,
		Emails:  b.Emails,
		Created: f.now(),
		owner:   t.email,
	}
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
		if validSlug(a.App, "invalid_suite") != nil || a.Host == "" {
			return nil, errf(400, "invalid_suite", "invalid app")
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
	_, s, e := f.ownerSuite(r, name)
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
	if validSlug(a.App, "invalid_suite") != nil || a.Host == "" {
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
			v := *g
			if v.Circles == nil {
				v.Circles = []string{}
			}
			if v.Groups == nil {
				v.Groups = []string{}
			}
			if v.Emails == nil {
				v.Emails = []string{}
			}
			out = append(out, v)
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
		Circles []string `json:"circles"`
		Groups  []string `json:"groups"`
		Emails  []string `json:"emails"`
		Circle  string   `json:"circle"`
		Email   string   `json:"email"`
	}
	if e := decode(r, &b); e != nil {
		return e
	}
	if b.Circle != "" && len(b.Circles) == 0 {
		b.Circles = []string{b.Circle}
	}
	if b.Email != "" && len(b.Emails) == 0 {
		b.Emails = []string{strings.ToLower(b.Email)}
	}
	if b.Circles == nil && b.Groups == nil && b.Emails == nil && b.Circle == "" && b.Email == "" {
		return errf(400, "invalid_share", "at least one audience rule required")
	}
	if b.Circle != "" && b.Email != "" {
		return errf(400, "invalid_share", "exactly one audience required")
	}
	for _, c := range b.Circles {
		if validSlug(c, "invalid_circle") != nil {
			return errf(400, "invalid_circle", "invalid circle %q", c)
		}
	}
	for _, g := range b.Groups {
		if validSlug(g, "invalid_group") != nil {
			return errf(400, "invalid_group", "invalid group %q", g)
		}
	}
	if b.Circles == nil {
		b.Circles = []string{}
	}
	if b.Groups == nil {
		b.Groups = []string{}
	}
	if b.Emails == nil {
		b.Emails = []string{}
	} else {
		for i, em := range b.Emails {
			b.Emails[i] = strings.ToLower(em)
		}
	}
	g := &SuiteShare{
		ID:      f.nextID("sgr"),
		Suite:   name,
		Circles: b.Circles,
		Groups:  b.Groups,
		Emails:  b.Emails,
		Created: f.now(),
		owner:   t.email,
	}
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

// --- Groups endpoints --------------------------------------------------------

func (f *Fake) listGroups(w http.ResponseWriter, r *http.Request) *apiError {
	tok, e := f.auth(r, ScopeMembersRead)
	if e != nil {
		return e
	}
	filterName := r.URL.Query().Get("name")
	var out []Group
	for _, g := range f.groups {
		if g.owner == tok.email {
			if filterName == "" || g.Name == filterName {
				cp := *g
				if cp.Members == nil {
					cp.Members = []string{}
				}
				out = append(out, cp)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []Group{}
	}
	writeJSON(w, 200, map[string]any{"groups": out})
	return nil
}

func (f *Fake) addGroupMembers(w http.ResponseWriter, r *http.Request, name string) *apiError {
	tok, e := f.auth(r, ScopeMembersWrite)
	if e != nil {
		return e
	}
	if validSlug(name, "invalid_group") != nil {
		return errf(400, "invalid_group", "invalid group name %q", name)
	}
	var b struct {
		Emails []string `json:"emails"`
	}
	if e := decode(r, &b); e != nil {
		return e
	}
	// owner email allowed; non-member email -> 400 {"error":"not_a_member"}
	for _, rawEmail := range b.Emails {
		email := strings.ToLower(rawEmail)
		if email == strings.ToLower(tok.email) {
			continue
		}
		if f.memberRow(tok.email, email) != nil {
			continue
		}
		return &apiError{status: 400, body: ErrorBody{Code: "not_a_member", Error: "not_a_member"}}
	}
	key := tok.email + "/" + name
	g := f.groups[key]
	if g == nil {
		g = &Group{Name: name, Members: []string{}, owner: tok.email}
		f.groups[key] = g
	}
	seen := map[string]bool{}
	for _, m := range g.Members {
		seen[strings.ToLower(m)] = true
	}
	for _, rawEmail := range b.Emails {
		email := strings.ToLower(rawEmail)
		if !seen[email] {
			g.Members = append(g.Members, email)
			seen[email] = true
		}
	}
	sort.Strings(g.Members)
	writeJSON(w, 200, map[string]any{"groups": []Group{*g}})
	return nil
}

func (f *Fake) removeGroupMember(w http.ResponseWriter, r *http.Request, name, email string) *apiError {
	tok, e := f.auth(r, ScopeMembersWrite)
	if e != nil {
		return e
	}
	key := tok.email + "/" + name
	g := f.groups[key]
	if g != nil {
		newMembers := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			if !strings.EqualFold(m, email) {
				newMembers = append(newMembers, m)
			}
		}
		g.Members = newMembers
	}
	w.WriteHeader(204)
	return nil
}

func (f *Fake) deleteGroup(w http.ResponseWriter, r *http.Request, name string) *apiError {
	tok, e := f.auth(r, ScopeMembersWrite)
	if e != nil {
		return e
	}
	delete(f.groups, tok.email+"/"+name)
	for _, s := range f.shares {
		if s.owner == tok.email {
			newGroups := make([]string, 0, len(s.Groups))
			for _, g := range s.Groups {
				if g != name {
					newGroups = append(newGroups, g)
				}
			}
			s.Groups = newGroups
		}
	}
	for _, ss := range f.suiteShares {
		if ss.owner == tok.email {
			newGroups := make([]string, 0, len(ss.Groups))
			for _, g := range ss.Groups {
				if g != name {
					newGroups = append(newGroups, g)
				}
			}
			ss.Groups = newGroups
		}
	}
	w.WriteHeader(204)
	return nil
}

// --- Rule matching and Who endpoint ------------------------------------------

type grantRule struct {
	circles []string
	groups  []string
	emails  []string
}

func (f *Fake) appGrantRules(owner, host, app string) []grantRule {
	var rules []grantRule
	for _, s := range f.shares {
		if s.Host == host && s.App == app && s.owner == owner {
			rules = append(rules, grantRule{s.Circles, s.Groups, s.Emails})
		}
	}
	for _, ss := range f.suiteShares {
		if ss.owner != owner {
			continue
		}
		suite := f.suites[owner+"/"+ss.Suite]
		if suite == nil {
			continue
		}
		for _, a := range suite.Apps {
			if (a.Host == host || a.Host == "*") && a.App == app {
				rules = append(rules, grantRule{ss.Circles, ss.Groups, ss.Emails})
				break
			}
		}
	}
	return rules
}

func (f *Fake) ruleMatches(circles, groups, emails []string, owner, memberEmail string) bool {
	memberEmail = strings.ToLower(memberEmail)
	ab := f.memberRow(owner, memberEmail)
	if ab == nil || ab.Status != "active" || !f.mutual(owner, memberEmail) {
		return false
	}
	if len(circles) > 0 {
		matched := false
		for _, c := range circles {
			if c == ab.Circle {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(groups) == 0 && len(emails) == 0 {
		return true
	}
	for _, gName := range groups {
		if g := f.groups[owner+"/"+gName]; g != nil {
			for _, gm := range g.Members {
				if strings.EqualFold(gm, memberEmail) {
					return true
				}
			}
		}
	}
	for _, em := range emails {
		if strings.EqualFold(em, memberEmail) {
			return true
		}
	}
	return false
}

// CanReach evaluates all grants against live owner membership and suite state.
func (f *Fake) CanReach(email, host, app string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[host]
	if h == nil {
		return false
	}
	if strings.EqualFold(email, h.OwnerEmail) {
		return true
	}
	rules := f.appGrantRules(h.OwnerEmail, host, app)
	if len(rules) == 0 {
		return false
	}
	email = strings.ToLower(email)
	for _, r := range rules {
		if f.ruleMatches(r.circles, r.groups, r.emails, h.OwnerEmail, email) {
			return true
		}
	}
	return false
}

func (f *Fake) whoAdmitted(w http.ResponseWriter, r *http.Request, host, app string) *apiError {
	t, e := f.shareOwner(r, host)
	if e != nil {
		return e
	}
	h := f.hosts[host]
	if h == nil {
		return errf(404, "not_found", "host not found")
	}
	rules := f.appGrantRules(t.email, host, app)
	admittedMap := map[string]bool{}
	if len(rules) > 0 {
		for _, m := range f.members {
			if m.owner == t.email && !strings.EqualFold(m.Email, t.email) {
				email := strings.ToLower(m.Email)
				for _, r := range rules {
					if f.ruleMatches(r.circles, r.groups, r.emails, t.email, email) {
						admittedMap[email] = true
						break
					}
				}
			}
		}
	}
	admitted := make([]string, 0, len(admittedMap))
	for em := range admittedMap {
		admitted = append(admitted, em)
	}
	sort.Strings(admitted)
	writeJSON(w, 200, map[string]any{
		"host":   host,
		"app":    app,
		"owner":  h.OwnerEmail,
		"emails": admitted,
	})
	return nil
}
