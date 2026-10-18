package kb

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/kblife"
	"github.com/qiangli/yoke/pkg/ref"
)

// `kb show REF [--json]` (Sprint 284 M1, design §5.1): one front door over
// every memory type. A `kind:id` (plus the `urn:dhnt:` and legacy `dhnt:`
// input spellings) resolves through the ref registry the embedding shell
// wires (bashy: lexicon.RefResolvers); a bare slug keeps today's kb page
// behaviour; a token whose prefix is not in the vocabulary is not a ref and
// is reported as such, never guessed.
//
// Wiring (done by the embedding shell, not here — pkg/kb stays an import
// leaf, TestKBIsALeaf): SetShowRegistry the registry to resolve through
// (nil = a registry with only the kb resolver, so `kb show kb:<slug>`
// works standalone), and SetShowReader one content reader per foreign kind.
// Content is rendered by the owning store's reader; nothing is copied into
// any kb ring. `sense:` is not a vocabulary kind: it is special-cased to the
// injected sense resolver (sense.go).

// ShowSchemaVersion is the discriminator every `kb show --json` envelope
// carries, on success AND error envelopes.
const ShowSchemaVersion = "bashy-kb-show-v1"

// Outcome is the §5.1 outcome vocabulary: one value in the envelope's
// `outcome` field; text mode maps it to the message.
type Outcome string

const (
	OutcomeFound    Outcome = "found"
	OutcomeNotFound Outcome = "not-found"
	OutcomeRedacted Outcome = "redacted"
	OutcomeExpired  Outcome = "expired"
	OutcomeDenied   Outcome = "denied"
	OutcomeError    Outcome = "error"
)

// ExitCode maps the outcome to the §5.1 exit code: found, redacted and
// expired read as success (0); not-found, denied and error fail (1).
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomeFound, OutcomeRedacted, OutcomeExpired:
		return 0
	default:
		return 1
	}
}

// ShowEnvelope is the exact `--json` shape (§5.1, discriminator spelled
// `schema_version` per operator override). Every field except the identity
// five is omitempty, so error envelopes carry no content and a v1 reader
// sees a v1 envelope.
type ShowEnvelope struct {
	SchemaVersion string   `json:"schema_version"`
	Ref           string   `json:"ref"`
	Kind          string   `json:"kind"`
	ID            string   `json:"id"`
	Outcome       string   `json:"outcome"`
	Title         string   `json:"title,omitempty"`
	Status        string   `json:"status,omitempty"`
	Where         string   `json:"where,omitempty"`
	Open          string   `json:"open,omitempty"`
	Content       string   `json:"content,omitempty"`
	Redacted      []string `json:"redacted,omitempty"`
	ObservedAt    string   `json:"observed_at,omitempty"`
	Freshness     string   `json:"freshness,omitempty"`
	Snapshot      string   `json:"snapshot,omitempty"`
	Error         string   `json:"error,omitempty"`
	// Lifecycle is set only when the page's resolved state is not valid
	// (superseded, expired, forgotten, not-yet-valid): the outcome stays
	// "found" — the page is still on disk — and this says why recall hides it.
	Lifecycle *ShowLifecycle `json:"lifecycle,omitempty"`
	// Proposal is set only on a `kb consolidate` candidate: its rule and
	// whether the source state it observed still holds (state "stale" with
	// the reasons when not).
	Proposal *ShowProposal `json:"proposal,omitempty"`
}

// ShowProposal is a consolidate candidate's audit status at show time.
type ShowProposal struct {
	ID          string   `json:"id"`
	Rule        string   `json:"rule"`
	RuleVersion int      `json:"rule_version"`
	State       string   `json:"state"` // pending|stale
	Reasons     []string `json:"reasons,omitempty"`
}

// ShowLifecycle is a page's resolved lifecycle state at show time, plus its
// tombstone when forgotten.
type ShowLifecycle struct {
	State     kblife.State      `json:"state"`
	Tombstone *kblife.Tombstone `json:"tombstone,omitempty"`
}

// ShowContent is what an owning store's reader returns for a resolved node.
// Outcome "" means found. ObservedAt/Freshness/Snapshot are the
// observation surface (§5.2); readers that have none leave them empty.
type ShowContent struct {
	Outcome    Outcome
	Content    string
	Redacted   []string
	ObservedAt string
	Freshness  string
	Snapshot   string
}

// ShowReader renders a resolved node to displayable content. A returned
// error wrapping ref.ErrNotFound reports outcome `not-found`; any other
// error reports outcome `error`.
type ShowReader func(n ref.Node) (ShowContent, error)

var (
	showRegistry *ref.Registry
	showReaders  = map[ref.Kind]ShowReader{}
)

// SetShowRegistry installs the registry `kb show` resolves `kind:id` refs
// through (bashy: lexicon.RefResolvers). Nil restores the standalone
// fallback: a registry with only the kb resolver.
func SetShowRegistry(g *ref.Registry) { showRegistry = g }

// SetShowReader installs the owning store's content reader for kind; a nil
// reader uninstalls it. A kind with a resolver but no reader reports
// outcome `error` naming the kind.
func SetShowReader(kind ref.Kind, r ShowReader) {
	if r == nil {
		delete(showReaders, kind)
		return
	}
	showReaders[kind] = r
}

// resetShowWiring restores the standalone fallback (tests).
func resetShowWiring() {
	showRegistry = nil
	showReaders = map[ref.Kind]ShowReader{}
}

func newShowCmd(dir, ring *string) *cobra.Command {
	var form string
	var jsonOut, okf bool
	cmd := &cobra.Command{
		Use:   "show <slug|#seq|uuid|kind:id>",
		Short: "Print one page (frontmatter + body) — by slug, running number, uuid, or kind:id ref",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			arg := strings.TrimSpace(args[0])
			if okf {
				return runShowOKF(c, arg, *dir, jsonOut)
			}
			if strings.HasPrefix(arg, "sense:") {
				return runShowSense(c, arg, jsonOut)
			}
			if r, err := ref.Parse(arg); err == nil {
				return runShowRef(c, r, *dir, *ring, form, jsonOut, false)
			} else if errors.Is(err, ref.ErrNotRef) {
				if strings.Contains(arg, ":") {
					return showFailure(c, jsonOut, showResult{
						Outcome: OutcomeError, Ref: arg,
						ErrText: fmt.Sprintf("kb: %s: not a ref — the prefix is not a kind (a bare kb slug has no colon)", arg),
					})
				}
				return runShowSlug(c, arg, *dir, *ring, form, jsonOut)
			} else {
				return showFailure(c, jsonOut, showResult{
					Outcome: OutcomeError, Ref: arg,
					ErrText: fmt.Sprintf("kb: %s: %s", arg, showParseError(rKindOf(arg), err)),
				})
			}
		},
	}
	cmd.Flags().StringVar(&form, "form", "", "require the page to be this form: note|page (kb pages only)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the §5.1 envelope instead of raw content")
	cmd.Flags().BoolVar(&okf, "okf", false, "render a foreign ref (episode|skill|out) as an OKF document, without ingesting it")
	return cmd
}

// rKindOf recovers the kind a failed parse was reaching for, for the error
// text. Parse files urn:/legacy unknown kinds under ref.Unknown with the
// raw tail in ID; canonical `kb:` empties carry their kind.
func rKindOf(arg string) string {
	if r, err := ref.Parse(arg); err == nil {
		return string(r.Kind)
	} else if r.Kind == ref.Unknown {
		if i := strings.IndexAny(r.ID, ":/"); i >= 0 {
			return r.ID[:i]
		}
		return r.ID
	}
	if scheme, _, ok := strings.Cut(strings.TrimSpace(arg), ":"); ok {
		return scheme
	}
	return ""
}

func showParseError(kind string, err error) string {
	switch {
	case errors.Is(err, ref.ErrUnknownKind):
		return fmt.Sprintf("unknown kind %q; kinds: %s", kind, strings.Join(ref.KindNames(), " "))
	case errors.Is(err, ref.ErrEmptyID):
		return fmt.Sprintf("a ref needs an id after the kind — <kind>:<id>, as in %s:<id>", kind)
	default:
		return err.Error()
	}
}

// showResult is one resolved-or-failed show, ready to print.
type showResult struct {
	Outcome    Outcome
	Ref        string
	Kind       string
	ID         string
	Title      string
	Status     string
	Where      string
	Open       string
	Content    string
	Redacted   []string
	ObservedAt string
	Freshness  string
	Snapshot   string
	ErrText    string
	Lifecycle  *ShowLifecycle
	Proposal   *ShowProposal
}

func (r showResult) envelope() ShowEnvelope {
	return ShowEnvelope{
		SchemaVersion: ShowSchemaVersion,
		Ref:           r.Ref,
		Kind:          r.Kind,
		ID:            r.ID,
		Outcome:       string(r.Outcome),
		Title:         r.Title,
		Status:        r.Status,
		Where:         r.Where,
		Open:          r.Open,
		Content:       r.Content,
		Redacted:      r.Redacted,
		ObservedAt:    r.ObservedAt,
		Freshness:     r.Freshness,
		Snapshot:      r.Snapshot,
		Error:         r.ErrText,
		Lifecycle:     r.Lifecycle,
		Proposal:      r.Proposal,
	}
}

// showFailure prints a failed show: the envelope on stdout in --json mode
// (no content field, outcome != found), a one-line message on stderr either
// way, and a non-nil error exactly when the outcome's exit code is 1.
func showFailure(c *cobra.Command, jsonOut bool, r showResult) error {
	if jsonOut {
		writeShowJSON(c.OutOrStdout(), r.envelope())
	}
	fmt.Fprintf(c.ErrOrStderr(), "kb: %s: %s: %s\n", r.Ref, r.Outcome, r.ErrText)
	if r.Outcome.ExitCode() != 0 {
		return fmt.Errorf("%s", r.ErrText)
	}
	return nil
}

// showSuccess prints a resolved show: raw content on stdout in text mode
// (human-first, like today's `kb show`), the envelope in --json mode, and a
// nil error exactly when the outcome's exit code is 0.
func showSuccess(c *cobra.Command, jsonOut bool, r showResult) error {
	if jsonOut {
		writeShowJSON(c.OutOrStdout(), r.envelope())
		if r.Outcome.ExitCode() != 0 {
			return fmt.Errorf("%s", r.ErrText)
		}
		return nil
	}
	switch r.Outcome {
	case OutcomeFound, OutcomeRedacted:
		if r.Content != "" {
			_, _ = io.WriteString(c.OutOrStdout(), r.Content)
		} else {
			fmt.Fprintf(c.OutOrStdout(), "kb: %s: withheld (%s)\n", r.Ref, strings.Join(r.Redacted, "; "))
		}
		return nil
	case OutcomeExpired:
		fmt.Fprintf(c.OutOrStdout(), "kb: %s: expired%s\n", r.Ref, showExpiredDetail(r))
		return nil
	default:
		fmt.Fprintf(c.ErrOrStderr(), "kb: %s: %s: %s\n", r.Ref, r.Outcome, r.ErrText)
		return fmt.Errorf("%s", r.ErrText)
	}
}

func showExpiredDetail(r showResult) string {
	var parts []string
	if r.Freshness != "" {
		parts = append(parts, r.Freshness)
	}
	if r.ObservedAt != "" {
		parts = append(parts, "observed "+r.ObservedAt)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func writeShowJSON(w io.Writer, env ShowEnvelope) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(env)
}

// runShowSlug is today's path, unchanged: a bare slug (no colon, so never a
// ref) opens the kb page by handle. --json wraps the same bytes in a found
// envelope.
func runShowSlug(c *cobra.Command, slug, dir, ring, form string, jsonOut bool) error {
	if form != "" && !ValidForm(form) {
		return showFailure(c, jsonOut, showResult{
			Outcome: OutcomeError, Ref: slug, Kind: "kb", ID: slug,
			ErrText: fmt.Sprintf("kb: invalid form %q (note|page|relation|code)", form),
		})
	}
	store := openRing(dir, ring)
	p, err := store.LoadByHandle(slug)
	if err != nil {
		return showFailure(c, jsonOut, showResult{
			Outcome: OutcomeNotFound, Ref: slug, Kind: "kb", ID: slug,
			ErrText: fmt.Sprintf("kb: %s: %s", slug, err),
		})
	}
	if form != "" && p.EffForm() != form {
		return showFailure(c, jsonOut, showResult{
			Outcome: OutcomeError, Ref: slug, Kind: "kb", ID: p.Slug,
			ErrText: fmt.Sprintf("kb: %s is form %s, not %s", p.Slug, p.EffForm(), form),
		})
	}
	b, err := os.ReadFile(store.PagePath(p.Slug))
	if err != nil {
		return showFailure(c, jsonOut, showResult{
			Outcome: OutcomeError, Ref: slug, Kind: "kb", ID: p.Slug,
			ErrText: err.Error(),
		})
	}
	// An OPEN is the use signal — the reader's judgement, not the
	// ranker's own output. See pkg/kb/activation.go.
	store.RecordOpen(p.Slug)
	life := pageLifecycle(c, store, p)
	prop := pageProposal(c, store, p)
	if jsonOut {
		return showSuccess(c, true, showResult{
			Outcome: OutcomeFound, Ref: "kb:" + p.Slug, Kind: "kb", ID: p.Slug,
			Title: p.Title, Status: p.Status, Content: string(b), Lifecycle: life, Proposal: prop,
		})
	}
	_, err = c.OutOrStdout().Write(b)
	return err
}

// runShowRef resolves a parsed `kind:id` through the registry and renders
// through the owning store's reader. kb: reads through today's store path
// (slug auto-detect, ring order repo→host); every other kind needs a wired
// reader.
func runShowRef(c *cobra.Command, r ref.Ref, dir, ring, form string, jsonOut, okf bool) error {
	fail := func(outcome Outcome, errText string) error {
		return showFailure(c, jsonOut, showResult{
			Outcome: outcome, Ref: r.String(), Kind: string(r.Kind), ID: r.ID, ErrText: errText,
		})
	}
	g := showRegistry
	if g == nil {
		g = ref.NewRegistry()
		RegisterRefs(g, dir, nil)
	}
	resolver, ok := g.Lookup(r.Kind)
	if !ok {
		return fail(OutcomeError, fmt.Sprintf("kb: kind %s: no resolver wired", r.Kind))
	}
	n, err := resolver.Resolve(r.ID)
	switch {
	case errors.Is(err, ref.ErrNotFound):
		return fail(OutcomeNotFound, err.Error())
	case err != nil:
		return fail(OutcomeError, err.Error())
	}
	if n.Ref == "" {
		n.Ref = ref.Format(n.Kind, n.ID)
	}
	if r.Kind == ref.KB {
		return runShowKBNode(c, n, dir, ring, form, jsonOut)
	}
	if form != "" {
		return fail(OutcomeError, fmt.Sprintf("kb: --form applies only to kb pages, not %s refs", r.Kind))
	}
	reader := showReaders[r.Kind]
	if reader == nil {
		return fail(OutcomeError, fmt.Sprintf("kb: kind %s: no content reader wired", r.Kind))
	}
	content, err := reader(n)
	switch {
	case errors.Is(err, ref.ErrNotFound):
		return fail(OutcomeNotFound, err.Error())
	case err != nil:
		return fail(OutcomeError, err.Error())
	}
	outcome := content.Outcome
	if outcome == "" {
		outcome = OutcomeFound
	}
	got := showResult{
		Outcome: outcome, Ref: n.Ref, Kind: string(n.Kind), ID: n.ID,
		Title: n.Title, Status: n.Status, Where: n.Where, Open: n.Open,
		Content: content.Content, Redacted: content.Redacted,
		ObservedAt: content.ObservedAt, Freshness: content.Freshness, Snapshot: content.Snapshot,
	}
	if outcome.ExitCode() != 0 {
		if got.ErrText = showOutcomeError(outcome, n, content); got.ErrText == "" {
			got.ErrText = n.Ref + ": " + string(outcome)
		}
		return showFailure(c, jsonOut, got)
	}
	if okf && got.Content != "" {
		doc, err := RenderOKFForeign(n, content)
		if err != nil {
			return fail(OutcomeError, err.Error())
		}
		got.Content = doc
	}
	return showSuccess(c, jsonOut, got)
}

// runShowOKF is `kb show REF --okf` (§5.1, §6.4): only a foreign ref of an
// OKF-rendered kind (episode|skill|out) qualifies; it resolves and reads
// exactly like the plain show and renders the content as an OKF document.
func runShowOKF(c *cobra.Command, arg, dir string, jsonOut bool) error {
	r, err := ref.Parse(arg)
	if err != nil || okfForeignTypes[r.Kind] == "" {
		return showFailure(c, jsonOut, showResult{
			Outcome: OutcomeError, Ref: arg,
			ErrText: fmt.Sprintf("kb: %s: --okf renders episode, skill and out refs", arg),
		})
	}
	return runShowRef(c, r, dir, "", "", jsonOut, true)
}

// runShowKBNode reads a resolved kb: node through today's store path, so the
// ring order, the form gate and the open-recording stay exactly today's.
func runShowKBNode(c *cobra.Command, n ref.Node, dir, ring, form string, jsonOut bool) error {
	fail := func(outcome Outcome, id, errText string) error {
		return showFailure(c, jsonOut, showResult{
			Outcome: outcome, Ref: n.Ref, Kind: string(n.Kind), ID: id,
			Title: n.Title, Status: n.Status, Where: n.Where, Open: n.Open, ErrText: errText,
		})
	}
	if form != "" && !ValidForm(form) {
		return fail(OutcomeError, n.ID, fmt.Sprintf("kb: invalid form %q (note|page|relation|code)", form))
	}
	store := openRing(dir, ring)
	p, err := store.LoadByHandle(n.ID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fail(OutcomeNotFound, n.ID, fmt.Sprintf("kb: %s: not found here", n.Ref))
	case errors.Is(err, ErrAmbiguous):
		return fail(OutcomeError, n.ID, err.Error())
	case err != nil:
		return fail(OutcomeError, n.ID, err.Error())
	}
	if form != "" && p.EffForm() != form {
		return fail(OutcomeError, p.Slug, fmt.Sprintf("kb: %s is form %s, not %s", p.Slug, p.EffForm(), form))
	}
	b, err := os.ReadFile(store.PagePath(p.Slug))
	if err != nil {
		return fail(OutcomeError, p.Slug, err.Error())
	}
	store.RecordOpen(p.Slug)
	return showSuccess(c, jsonOut, showResult{
		Outcome: OutcomeFound, Ref: n.Ref, Kind: string(n.Kind), ID: n.ID,
		Title: n.Title, Status: n.Status, Where: n.Where, Open: n.Open,
		Content: string(b), Lifecycle: pageLifecycle(c, store, p), Proposal: pageProposal(c, store, p),
	})
}

// pageProposal reports a consolidate candidate's audit status; nil on any
// other page. A stale proposal is also noted on stderr.
func pageProposal(c *cobra.Command, store *Store, p *Page) *ShowProposal {
	if p.Proposal == nil {
		return nil
	}
	sp := &ShowProposal{ID: p.Proposal.ID, Rule: p.Proposal.Rule, RuleVersion: p.Proposal.RuleVersion, State: "pending"}
	if p.Status != StatusCandidate {
		sp.State = p.Status
		return sp
	}
	reasons, err := store.ProposalStale(p)
	if err != nil {
		sp.State, sp.Reasons = "unknown", []string{err.Error()}
		return sp
	}
	if len(reasons) > 0 {
		sp.State, sp.Reasons = "stale", reasons
		fmt.Fprintf(c.ErrOrStderr(), "kb: %s: proposal stale: %s\n", p.Slug, strings.Join(reasons, "; "))
	}
	return sp
}

// pageLifecycle resolves p now; nil when it is simply valid, so a valid page's
// envelope is unchanged. A non-valid state is also noted on stderr in text mode.
func pageLifecycle(c *cobra.Command, store *Store, p *Page) *ShowLifecycle {
	tomb, _ := store.Tombstone(p.Slug)
	state := PageState(p, tomb, time.Now())
	if state == kblife.StateValid {
		return nil
	}
	note := fmt.Sprintf("kb: %s: lifecycle %s", p.Slug, state)
	if tomb != nil {
		note += fmt.Sprintf(" at %s by %s: %s", tomb.At.Format(time.RFC3339), tomb.By, tomb.Reason)
	}
	fmt.Fprintln(c.ErrOrStderr(), note)
	return &ShowLifecycle{State: state, Tombstone: tomb}
}

// showOutcomeError names a non-found reader outcome for the error line and
// the envelope's error field.
func showOutcomeError(outcome Outcome, n ref.Node, content ShowContent) string {
	switch outcome {
	case OutcomeDenied:
		return fmt.Sprintf("kb: %s: the requesting principal may not read it", n.Ref)
	case OutcomeExpired:
		detail := showExpiredDetail(showResult{Ref: n.Ref, Freshness: content.Freshness, ObservedAt: content.ObservedAt})
		return fmt.Sprintf("kb: %s: expired%s — never a current fact", n.Ref, detail)
	case OutcomeRedacted:
		return fmt.Sprintf("kb: %s: withheld (%s)", n.Ref, strings.Join(content.Redacted, "; "))
	default:
		return ""
	}
}

// runShowSense special-cases the `sense:` prefix (not a vocabulary kind) to
// the injected sense resolver.
func runShowSense(c *cobra.Command, input string, jsonOut bool) error {
	id, content, err := showSenseReading(input)
	if err != nil {
		outcome := OutcomeError
		if errors.Is(err, ref.ErrNotFound) {
			outcome = OutcomeNotFound
		}
		return showFailure(c, jsonOut, showResult{
			Outcome: outcome, Ref: strings.TrimSpace(input), Kind: "sense", ID: id, ErrText: err.Error(),
		})
	}
	res := showResult{
		Outcome: content.Outcome, Ref: "sense:" + id, Kind: "sense", ID: id,
		Content: content.Content, Redacted: content.Redacted,
		ObservedAt: content.ObservedAt, Freshness: content.Freshness, Snapshot: content.Snapshot,
	}
	if content.Outcome.ExitCode() != 0 {
		switch content.Outcome {
		case OutcomeNotFound:
			res.ErrText = fmt.Sprintf("sense: %s: no reading held", id)
		case OutcomeDenied:
			res.ErrText = fmt.Sprintf("sense: %s: the requesting principal may not read it", id)
		case OutcomeExpired:
			res.ErrText = fmt.Sprintf("sense: %s: expired (%s) — never a current fact", id, content.Freshness)
		default:
			res.ErrText = fmt.Sprintf("sense: %s: %s", id, content.Outcome)
		}
		return showFailure(c, jsonOut, res)
	}
	return showSuccess(c, jsonOut, res)
}

// showSenseReading splits the sense id out of the input and runs the mapping
// in sense.go: a resolver ref.ErrNotFound surfaces here so the caller reports
// `not-found`; every other resolver error surfaces for outcome `error`.
func showSenseReading(input string) (string, ShowContent, error) {
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), "sense:"))
	if id == "" {
		return "", ShowContent{}, errEmptySenseID
	}
	if senseResolver == nil {
		return id, ShowContent{}, errors.New("no sense resolver wired")
	}
	rd, err := senseResolver(id)
	if err != nil {
		return id, ShowContent{}, err
	}
	if !senseAllowed(rd, RequestPrincipal()) {
		return id, ShowContent{Outcome: OutcomeDenied}, nil
	}
	freshness := SenseFreshness(rd, senseClock())
	base := ShowContent{
		ObservedAt: rd.ObservedAt,
		Freshness:  freshness,
		Snapshot:   rd.Snapshot,
	}
	if freshness == SenseFreshnessFresh {
		base.Outcome = OutcomeFound
		base.Content = string(rd.Value)
		return id, base, nil
	}
	base.Outcome = OutcomeExpired
	return id, base, nil
}
