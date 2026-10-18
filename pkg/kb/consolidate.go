package kb

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/kblife"
)

// kb consolidate (design §7) — the sleep-time pass. It is FISH over existing
// rods: it reads pages, journal observe events and graph relations through the
// store's own readers, applies deterministic rules shipped as editable data
// (consolidate_rules.yaml, replaced wholesale by --rules), and writes ONLY
// candidate notes. It never promotes and never touches an existing page, so a
// proposal is a page with a `proposal:` block naming its rule, the sources it
// read and the state each source was in. That block is the whole audit
// contract: the same rule over the same source state yields the same proposal
// id (no duplicate on rerun), and a pending proposal whose source has since
// changed, been superseded or forgotten resolves as stale — at consumption,
// never by rewriting the candidate.

//go:embed consolidate_rules.yaml
var defaultConsolidateRules []byte

// ConsolidateSchemaVersion discriminates the `kb consolidate --json` report.
const ConsolidateSchemaVersion = "bashy-kb-consolidate-v1"

// consolidateClock is "now" for --since; tests pin it.
var consolidateClock = time.Now

// ConsolidateRules is the rule data file.
type ConsolidateRules struct {
	Schema        string   `yaml:"schema"`
	ClaimPattern  string   `yaml:"claim_pattern"`
	UnknownValues []string `yaml:"unknown_values"`
	Failure       struct {
		Kinds    []string `yaml:"kinds"`
		Outcomes []string `yaml:"outcomes"`
	} `yaml:"failure"`
	Rules []ConsolidateRule `yaml:"rules"`

	claim *regexp.Regexp
}

// ConsolidateRule is one rule: a matcher the engine implements plus the data
// that tunes and words it.
type ConsolidateRule struct {
	ID       string `yaml:"id"`
	Version  int    `yaml:"version"`
	Match    string `yaml:"match"`
	Min      int    `yaml:"min"`
	Type     string `yaml:"type"`
	Relation string `yaml:"relation"`
	Title    string `yaml:"title"`
	Body     string `yaml:"body"`

	title, body *template.Template
}

var consolidateMatchers = []string{"failure", "repeated-claim", "claim-supports", "note-in-failed-episode", "differing-claim"}

// ParseConsolidateRules parses and checks a rule data file.
func ParseConsolidateRules(b []byte) (*ConsolidateRules, error) {
	var rs ConsolidateRules
	if err := yaml.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("kb: consolidate rules: %w", err)
	}
	if rs.Schema != "bashy-kb-consolidate-rules-v1" {
		return nil, fmt.Errorf("kb: consolidate rules: schema %q, want bashy-kb-consolidate-rules-v1", rs.Schema)
	}
	re, err := regexp.Compile(rs.ClaimPattern)
	if err != nil {
		return nil, fmt.Errorf("kb: consolidate rules: claim_pattern: %w", err)
	}
	if re.NumSubexp() < 2 {
		return nil, fmt.Errorf("kb: consolidate rules: claim_pattern needs two groups (subject, value)")
	}
	rs.claim = re
	seen := map[string]bool{}
	for i := range rs.Rules {
		r := &rs.Rules[i]
		switch {
		case r.ID == "":
			return nil, fmt.Errorf("kb: consolidate rules: rule %d has no id", i+1)
		case seen[r.ID]:
			return nil, fmt.Errorf("kb: consolidate rules: duplicate rule id %q", r.ID)
		case r.Version < 1:
			return nil, fmt.Errorf("kb: consolidate rules: %s: version must be >= 1", r.ID)
		case !slices.Contains(consolidateMatchers, r.Match):
			return nil, fmt.Errorf("kb: consolidate rules: %s: unknown match %q (%s)", r.ID, r.Match, strings.Join(consolidateMatchers, "|"))
		case !ValidType(r.Type):
			return nil, fmt.Errorf("kb: consolidate rules: %s: invalid type %q", r.ID, r.Type)
		}
		seen[r.ID] = true
		if r.Min < 1 {
			r.Min = 1
		}
		if r.title, err = template.New(r.ID + ".title").Option("missingkey=error").Parse(r.Title); err != nil {
			return nil, fmt.Errorf("kb: consolidate rules: %s: title: %w", r.ID, err)
		}
		if r.body, err = template.New(r.ID + ".body").Option("missingkey=error").Parse(r.Body); err != nil {
			return nil, fmt.Errorf("kb: consolidate rules: %s: body: %w", r.ID, err)
		}
	}
	return &rs, nil
}

func (rs *ConsolidateRules) unknown(v string) bool {
	for _, u := range rs.UnknownValues {
		if strings.EqualFold(strings.TrimSpace(v), u) {
			return true
		}
	}
	return strings.TrimSpace(v) == ""
}

// claimOf extracts (subject, value) from text, ok=false when text is no claim.
func (rs *ConsolidateRules) claimOf(text string) (subject, value string, ok bool) {
	m := rs.claim.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", "", false
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), true
}

// Proposal is the audit block a consolidate candidate carries in its
// frontmatter: which rule proposed it, from which sources, observing which
// source state. ID = hash(rule, rule version, sorted source refs, state
// digest).
type Proposal struct {
	ID          string           `yaml:"id" json:"id"`
	Rule        string           `yaml:"rule" json:"rule"`
	RuleVersion int              `yaml:"rule_version" json:"rule_version"`
	Kind        string           `yaml:"kind" json:"kind"` // lesson|fact|relation|contradicts
	Subject     string           `yaml:"subject,omitempty" json:"subject,omitempty"`
	Relation    string           `yaml:"relation,omitempty" json:"relation,omitempty"`
	From        string           `yaml:"from,omitempty" json:"from,omitempty"`
	To          string           `yaml:"to,omitempty" json:"to,omitempty"`
	Sources     []ProposalSource `yaml:"sources" json:"sources"`
	StateDigest string           `yaml:"state_digest" json:"state_digest"`
}

// ProposalSource is one source as the proposal observed it. Digest covers the
// source's content hash, status, valid_to and tombstone; State is its
// kblife-resolved state then.
type ProposalSource struct {
	Ref        string `yaml:"ref" json:"ref"`
	State      string `yaml:"state" json:"state"`
	Digest     string `yaml:"digest" json:"digest"`
	ObservedAt string `yaml:"observed_at,omitempty" json:"observed_at,omitempty"`
	Episode    string `yaml:"episode,omitempty" json:"episode,omitempty"`
}

// sourceRec is one readable source: a page, a journal observe event or a
// graph relation.
type sourceRec struct {
	ProposalSource
	text      string // claim / summary text
	at        time.Time
	failure   bool
	signature string // failure grouping key
	page      *Page  // set for kb: pages
	fact      bool   // a validated page — an existing fact, never rewritten
}

func (s sourceRec) line() string {
	if s.text == "" {
		return s.Ref
	}
	return s.Ref + ": " + s.text
}

func shortHash(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// stateDigest is the digest of one source's lifecycle-relevant state.
func stateDigest(content, status, validTo string, tomb *kblife.Tombstone) string {
	ts := ""
	if tomb != nil {
		ts = tomb.At.UTC().Format(time.RFC3339) + " " + tomb.Reason
	}
	return shortHash("content="+content, "status="+status, "valid_to="+validTo, "tombstone="+ts)
}

// consolidateInputs is everything the rules read, plus the current state of
// every ref — the latter is what stale detection compares against.
type consolidateInputs struct {
	sources []sourceRec          // live, non-proposal sources the rules see
	state   map[string]sourceRec // every known ref's current state
	tombs   map[string]kblife.Tombstone
	pending []*Page // consolidate candidates (pages with a proposal block)
	ids     map[string]*Page
}

// loadConsolidateInputs reads the store through its existing readers: List +
// Tombstones (pages, resolved by kblife), the journal's observe events, and
// LiveRelations.
func (s *Store) loadConsolidateInputs(rules *ConsolidateRules, now time.Time) (*consolidateInputs, error) {
	in := &consolidateInputs{state: map[string]sourceRec{}, ids: map[string]*Page{}}
	pages, err := s.List()
	if err != nil {
		return nil, err
	}
	if in.tombs, err = s.Tombstones(); err != nil {
		return nil, err
	}
	for _, p := range pages {
		var tomb *kblife.Tombstone
		if t, ok := in.tombs[p.Slug]; ok {
			tomb = &t
		}
		st := PageState(p, tomb, now)
		obs := p.Updated
		if obs == "" {
			obs = p.Created
		}
		rec := sourceRec{
			ProposalSource: ProposalSource{
				Ref: "kb:" + p.Slug, State: string(st), ObservedAt: obs,
				Digest: stateDigest(shortHash(p.Title, p.Description, p.Body), p.Status, p.ValidTo, tomb),
			},
			text: p.Title, at: parseRFC3339(obs), page: p,
			fact: p.Status == StatusValidated,
		}
		if p.Source != nil {
			rec.Episode = p.Source.Episode
		}
		in.state[rec.Ref] = rec
		if p.Proposal != nil {
			in.pending = append(in.pending, p)
			in.ids[p.Proposal.ID] = p
			continue
		}
		if kblife.Visible(st) {
			in.sources = append(in.sources, rec)
		}
	}
	events, err := s.observeEvents()
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		ev := e.ev
		rec := sourceRec{
			ProposalSource: ProposalSource{
				Ref: "observe:" + ev.ID, State: string(kblife.StateValid), ObservedAt: ev.At, Episode: ev.Episode,
				Digest: stateDigest(shortHash(e.raw), "observed", "", nil),
			},
			text: ev.Summary, at: parseRFC3339(ev.At),
		}
		rec.failure = slices.Contains(rules.Failure.Kinds, ev.Kind) || (ev.Ran && !ev.Passed) || ev.ExitCode != 0
		rec.signature = ev.Command
		if rec.signature == "" {
			rec.signature = ev.Ref
		}
		in.state[rec.Ref] = rec
		in.sources = append(in.sources, rec)
	}
	rels, err := s.LiveRelations()
	if err != nil {
		return nil, err
	}
	for _, r := range rels {
		if r.Op != "note" && r.Op != "observe" {
			continue
		}
		raw, _ := json.Marshal(r)
		at := ""
		if !r.At.IsZero() {
			at = r.At.UTC().Format(time.RFC3339)
		}
		rec := sourceRec{
			ProposalSource: ProposalSource{
				Ref: "graph:" + r.ID, State: string(kblife.StateValid), ObservedAt: at, Episode: r.Episode,
				Digest: stateDigest(shortHash(string(raw)), "live", "", nil),
			},
			text: r.Text, at: r.At, signature: r.Target,
			failure: r.Op == "observe" && slices.Contains(rules.Failure.Outcomes, r.Outcome),
		}
		in.state[rec.Ref] = rec
		in.sources = append(in.sources, rec)
	}
	sort.SliceStable(in.sources, func(i, j int) bool { return in.sources[i].Ref < in.sources[j].Ref })
	return in, nil
}

type rawObserve struct {
	ev  observeEvent
	raw string
}

// observeEvents replays the journal's observe events in order. On the
// owner-scoped agent ring only the caller's own events are read.
func (s *Store) observeEvents() ([]rawObserve, error) {
	f, err := os.Open(s.journalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []rawObserve
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev observeEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "observe" || ev.ID == "" {
			continue
		}
		if s.ownerScoped && (s.owner == "" || ev.Tool != s.owner) {
			continue
		}
		out = append(out, rawObserve{ev: ev, raw: sc.Text()})
	}
	return out, sc.Err()
}

// ConsolidateItem is one proposal as reported: its audit block plus the
// candidate it would write. Status is "new" (would be / was written) or
// "exists" (a candidate with this id is already on disk — no duplicate).
type ConsolidateItem struct {
	Proposal
	Slug   string `json:"slug"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Status string `json:"status"`
}

// StaleProposal is a pending candidate whose observed source state no longer
// holds. Nothing is rewritten; the candidate just cannot be accepted as is.
type StaleProposal struct {
	ID      string   `json:"id"`
	Slug    string   `json:"slug"`
	Rule    string   `json:"rule"`
	Reasons []string `json:"reasons"`
}

// ConsolidateReport is the `kb consolidate --json` shape.
type ConsolidateReport struct {
	SchemaVersion string            `json:"schema_version"`
	Rules         string            `json:"rules"`
	Since         string            `json:"since,omitempty"`
	Applied       bool              `json:"applied"`
	Proposals     []ConsolidateItem `json:"proposals"`
	Stale         []StaleProposal   `json:"stale"`
}

// tmplData is what a rule's title/body templates see.
type tmplData struct {
	Subject, Value, Relation, From, To string
	Values                             []string
	Count                              int
	Lines                              []string
}

// Consolidate runs rules over the store and reports proposals and stale
// pending candidates. since (zero = no bound) limits which sources are read;
// validated facts are always read, since an old fact is exactly what a new
// claim may contradict. It writes nothing.
func (s *Store) Consolidate(rules *ConsolidateRules, since time.Time) (ConsolidateReport, error) {
	now := consolidateClock()
	in, err := s.loadConsolidateInputs(rules, now)
	if err != nil {
		return ConsolidateReport{}, err
	}
	var srcs []sourceRec
	for _, r := range in.sources {
		if !since.IsZero() && !r.fact && !r.at.IsZero() && r.at.Before(since) {
			continue
		}
		srcs = append(srcs, r)
	}
	rep := ConsolidateReport{SchemaVersion: ConsolidateSchemaVersion, Proposals: []ConsolidateItem{}, Stale: []StaleProposal{}}
	for i := range rules.Rules {
		items, err := runRule(rules, &rules.Rules[i], srcs)
		if err != nil {
			return ConsolidateReport{}, err
		}
		for _, it := range items {
			it.Status = "new"
			if _, ok := in.ids[it.ID]; ok {
				it.Status = "exists"
			}
			rep.Proposals = append(rep.Proposals, it)
		}
	}
	for _, p := range in.pending {
		if p.Status != StatusCandidate {
			continue
		}
		if reasons := proposalStaleReasons(p.Proposal, in.state, in.tombs); len(reasons) > 0 {
			rep.Stale = append(rep.Stale, StaleProposal{ID: p.Proposal.ID, Slug: p.Slug, Rule: p.Proposal.Rule, Reasons: reasons})
		}
	}
	return rep, nil
}

// proposalStaleReasons compares each recorded source with its current state.
func proposalStaleReasons(pr *Proposal, state map[string]sourceRec, tombs map[string]kblife.Tombstone) []string {
	var out []string
	for _, src := range pr.Sources {
		cur, ok := state[src.Ref]
		switch {
		case !ok:
			out = append(out, src.Ref+": missing (removed or forgotten)")
		case cur.State != src.State:
			why := fmt.Sprintf("%s: %s (was %s)", src.Ref, cur.State, src.State)
			if cur.State == string(kblife.StateForgotten) {
				if t, ok := tombs[strings.TrimPrefix(src.Ref, "kb:")]; ok {
					why += ": " + t.Reason
				}
			}
			out = append(out, why)
		case cur.Digest != src.Digest:
			out = append(out, src.Ref+": changed since proposed")
		}
	}
	return out
}

// ProposalStale reports why p's proposal is stale in store (nil = still
// pending as proposed, or p is no proposal). kb show and kb validate use it.
func (s *Store) ProposalStale(p *Page) ([]string, error) {
	if p == nil || p.Proposal == nil {
		return nil, nil
	}
	rules, err := ParseConsolidateRules(defaultConsolidateRules)
	if err != nil {
		return nil, err
	}
	in, err := s.loadConsolidateInputs(rules, consolidateClock())
	if err != nil {
		return nil, err
	}
	return proposalStaleReasons(p.Proposal, in.state, in.tombs), nil
}

func newItem(rule *ConsolidateRule, kind string, srcs []sourceRec, d tmplData) (ConsolidateItem, error) {
	srcs = slices.Clone(srcs)
	sort.SliceStable(srcs, func(i, j int) bool { return srcs[i].Ref < srcs[j].Ref })
	srcs = slices.CompactFunc(srcs, func(a, b sourceRec) bool { return a.Ref == b.Ref })
	pr := Proposal{Rule: rule.ID, RuleVersion: rule.Version, Kind: kind, Subject: d.Subject, Relation: rule.Relation, From: d.From, To: d.To}
	var digestParts, refs []string
	for _, s := range srcs {
		pr.Sources = append(pr.Sources, s.ProposalSource)
		refs = append(refs, s.Ref)
		digestParts = append(digestParts, s.Ref+"="+s.Digest)
		d.Lines = append(d.Lines, s.line())
	}
	switch kind {
	case "contradicts":
		pr.Relation = "contradicts"
	case "lesson", "fact":
		pr.Relation = ""
	}
	pr.StateDigest = shortHash(digestParts...)
	pr.ID = shortHash(rule.ID, fmt.Sprint(rule.Version), strings.Join(refs, "\x00"), pr.StateDigest)
	d.Relation = pr.Relation
	d.Count = len(srcs)
	var title, body bytes.Buffer
	if err := rule.title.Execute(&title, d); err != nil {
		return ConsolidateItem{}, fmt.Errorf("kb: consolidate rule %s: title: %w", rule.ID, err)
	}
	if err := rule.body.Execute(&body, d); err != nil {
		return ConsolidateItem{}, fmt.Errorf("kb: consolidate rule %s: body: %w", rule.ID, err)
	}
	return ConsolidateItem{
		Proposal: pr, Slug: "consolidate-" + pr.ID, Type: rule.Type,
		Title: strings.TrimSpace(title.String()), Body: strings.TrimSpace(body.String()),
	}, nil
}

type claimRec struct {
	sourceRec
	subject, value string
}

func claims(rules *ConsolidateRules, srcs []sourceRec, facts bool) []claimRec {
	var out []claimRec
	for _, s := range srcs {
		if s.fact != facts || s.failure {
			continue
		}
		subj, val, ok := rules.claimOf(s.text)
		if !ok || rules.unknown(val) {
			continue
		}
		out = append(out, claimRec{s, subj, val})
	}
	return out
}

// runRule applies one rule's matcher. Outputs are in a deterministic order.
func runRule(rules *ConsolidateRules, rule *ConsolidateRule, srcs []sourceRec) ([]ConsolidateItem, error) {
	var items []ConsolidateItem
	add := func(kind string, group []sourceRec, d tmplData) error {
		it, err := newItem(rule, kind, group, d)
		if err == nil {
			items = append(items, it)
		}
		return err
	}
	switch rule.Match {
	case "failure":
		groups := map[string][]sourceRec{}
		for _, s := range srcs {
			if s.failure {
				groups[s.signature] = append(groups[s.signature], s)
			}
		}
		for _, sig := range sortedKeys(groups) {
			if g := groups[sig]; len(g) >= rule.Min {
				if err := add("lesson", g, tmplData{Subject: sig}); err != nil {
					return nil, err
				}
			}
		}
	case "repeated-claim":
		groups := map[[2]string][]sourceRec{}
		for _, c := range claims(rules, srcs, false) {
			k := [2]string{c.subject, c.value}
			groups[k] = append(groups[k], c.sourceRec)
		}
		keys := make([][2]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i][0] != keys[j][0] {
				return keys[i][0] < keys[j][0]
			}
			return keys[i][1] < keys[j][1]
		})
		for _, k := range keys {
			if g := groups[k]; len(g) >= rule.Min {
				if err := add("fact", g, tmplData{Subject: k[0], Value: k[1]}); err != nil {
					return nil, err
				}
			}
		}
	case "claim-supports":
		facts := claims(rules, srcs, true)
		for _, c := range claims(rules, srcs, false) {
			for _, f := range facts {
				if f.subject == c.subject && f.value == c.value {
					d := tmplData{Subject: c.subject, Value: c.value, From: c.Ref, To: f.Ref}
					if err := add("relation", []sourceRec{c.sourceRec, f.sourceRec}, d); err != nil {
						return nil, err
					}
				}
			}
		}
	case "note-in-failed-episode":
		for _, p := range srcs {
			if p.page == nil || p.Episode == "" {
				continue
			}
			for _, f := range srcs {
				if f.failure && f.Episode == p.Episode {
					if err := add("relation", []sourceRec{p, f}, tmplData{From: p.Ref, To: f.Ref}); err != nil {
						return nil, err
					}
				}
			}
		}
	case "differing-claim":
		bySubj := map[string][]claimRec{}
		for _, c := range append(claims(rules, srcs, false), claims(rules, srcs, true)...) {
			bySubj[c.subject] = append(bySubj[c.subject], c)
		}
		for _, subj := range sortedKeys(bySubj) {
			var vals []string
			var group []sourceRec
			for _, c := range bySubj[subj] {
				if !slices.Contains(vals, c.value) {
					vals = append(vals, c.value)
				}
				group = append(group, c.sourceRec)
			}
			if len(vals) < 2 {
				continue
			}
			sort.Strings(vals)
			if err := add("contradicts", group, tmplData{Subject: subj, Values: vals}); err != nil {
				return nil, err
			}
		}
	}
	return items, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// applyConsolidate writes each new proposal as a candidate note — the same
// shape and redaction gate as `kb note add --candidate`. It returns the slugs
// written. Existing pages are never touched.
func (s *Store) applyConsolidate(rep *ConsolidateReport, ring string) ([]string, error) {
	var wrote []string
	for i := range rep.Proposals {
		it := &rep.Proposals[i]
		if it.Status != "new" {
			continue
		}
		if ring != "agent" {
			if err := shareableWriteCheck(it.Title + "\n" + it.Body); err != nil {
				return wrote, err
			}
		}
		pr := it.Proposal
		p := &Page{
			Slug:        it.Slug,
			Form:        FormNote,
			Type:        it.Type,
			Title:       it.Title,
			Description: "kb consolidate proposal (" + pr.Rule + ") — candidate only; validate or ignore",
			Tags:        []string{"consolidate", "rule:" + pr.Rule},
			Status:      StatusCandidate,
			Source:      &Source{Tool: ToolID(), Host: HostID(), Episode: EpisodeID(), ParentEpisode: ParentEpisodeID()},
			Body:        it.Body,
			Proposal:    &pr,
		}
		if err := s.Write(p, "consolidate"); err != nil {
			return wrote, err
		}
		wrote = append(wrote, p.Slug)
	}
	return wrote, nil
}

// parseSince reads --since: a Go duration (72h) or Nd days back from now, or
// a date (2006-01-02) / RFC 3339 instant.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err == nil && fmt.Sprint(days) == n {
			return now.AddDate(0, 0, -days), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("kb: --since %q: want a duration (72h, 7d) or a date (2006-01-02, RFC 3339)", s)
}

// --- consolidate ---------------------------------------------------------

func newConsolidateCmd(dir, ring *string) *cobra.Command {
	var since, rulesFile string
	var jsonOut, apply bool
	cmd := &cobra.Command{
		Use:   "consolidate [--since DUR|DATE] [--rules FILE] [--json] [--apply]",
		Short: "Propose lesson/fact/relation candidates and contradicts flags from recent episodes, events and notes",
		Long: `The sleep-time pass: deterministic rules (editable data — the embedded
default, or --rules FILE) read pages, journal observe events and graph notes
and PROPOSE candidates: lessons from failures, facts from repeated claims,
caused-by / supports relations, contradicts flags for differing claims.

Every output is a candidate note carrying its rule, sources and the source
state it observed. It never promotes and never edits an existing page. Without
--apply it is a dry run; --apply writes the new candidates. Rerunning on the
same sources proposes the same ids and writes nothing new. A pending proposal
whose source changed, was superseded or forgotten is reported stale (and kb
validate refuses it).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			raw, label := defaultConsolidateRules, "embedded"
			if strings.TrimSpace(rulesFile) != "" {
				b, err := os.ReadFile(rulesFile)
				if err != nil {
					return err
				}
				raw, label = b, rulesFile
			}
			rules, err := ParseConsolidateRules(raw)
			if err != nil {
				return err
			}
			cut, err := parseSince(since, consolidateClock())
			if err != nil {
				return err
			}
			store := openRing(*dir, *ring)
			rep, err := store.Consolidate(rules, cut)
			if err != nil {
				return err
			}
			rep.Rules, rep.Since = label, strings.TrimSpace(since)
			var wrote []string
			if apply {
				rep.Applied = true
				if wrote, err = store.applyConsolidate(&rep, *ring); err != nil {
					return err
				}
			}
			out := c.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			mode := "dry run (use --apply to write candidates)"
			if apply {
				mode = fmt.Sprintf("applied: %d candidate(s) written", len(wrote))
			}
			fmt.Fprintf(out, "kb consolidate — %d proposal(s), %d stale; %s\n", len(rep.Proposals), len(rep.Stale), mode)
			for _, it := range rep.Proposals {
				fmt.Fprintf(out, "  %s %-11s %s  %s  [%s v%d]\n", it.Status, it.Kind, it.Slug, it.Title, it.Rule, it.RuleVersion)
			}
			for _, st := range rep.Stale {
				fmt.Fprintf(out, "  stale %s [%s]: %s\n", st.Slug, st.Rule, strings.Join(st.Reasons, "; "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only read sources observed since DUR (72h, 7d) or DATE")
	cmd.Flags().StringVar(&rulesFile, "rules", "", "rule data file replacing the embedded default")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the bashy-kb-consolidate-v1 report")
	cmd.Flags().BoolVar(&apply, "apply", false, "write the new proposals as candidate notes")
	return cmd
}
