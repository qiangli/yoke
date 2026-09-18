// Package admission builds bounded projections of already-prepared input.
//
// It owns no messages and knows nothing about their stores. Callers adapt
// durable records into Items, then commit the returned Prepared value only
// after the rendered text was accepted by its destination.
package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultTurnBytes is the combined automatic coordination allowance at one
	// turn boundary.
	DefaultTurnBytes = 4 * 1024
	// DefaultPreviewBytes bounds one inline body. Larger bodies need a stable
	// retrieval reference and are represented by a header instead.
	DefaultPreviewBytes = 512
	// OverflowSchemaVersion versions the machine-readable overflow line.
	OverflowSchemaVersion = "bashy-coordination-overflow-v1"
)

// Priority is ordered from most to least important. Lower values win.
type Priority int

const (
	PriorityUrgent Priority = iota + 1
	PriorityResponse
	PriorityDecision
	PriorityDirected
	PriorityInformational
)

// Item is one record prepared by an authoritative-store adapter.
// ArtifactRef must be a stable command or path that can retrieve Body exactly.
type Item struct {
	Source      string
	ID          string
	Sequence    int64
	From        string
	To          string
	Topic       string
	Priority    Priority
	Body        string
	ArtifactRef string
	// OverflowRef opens the source's unread view when this item cannot receive
	// even an individual header. It may be shared by every item from a source.
	OverflowRef string
	Acknowledge func() error
}

// Options controls one composition boundary.
type Options struct {
	BudgetBytes  int
	PreviewBytes int
}

// Report contains only byte/item accounting and content identities. It is safe
// to copy into telemetry; message text and metadata are deliberately absent.
type Report struct {
	SchemaVersion      string
	ContentDigest      string
	InputItems         int
	InputBytes         int
	AdmittedItems      int
	FullItems          int
	ReferencedItems    int
	OmittedItems       int
	OmittedBytes       int
	UnrepresentedItems int
	UnrepresentedBytes int
	RenderedBytes      int
}

// Overflow is emitted as versioned JSON when any body is not inline.
type Overflow struct {
	SchemaVersion      string           `json:"schema_version"`
	ContentDigest      string           `json:"content_digest"`
	OmittedItems       int              `json:"omitted_items"`
	OmittedBytes       int              `json:"omitted_bytes"`
	ReferencedItems    int              `json:"referenced_items"`
	UnrepresentedItems int              `json:"unrepresented_items"`
	UnrepresentedBytes int              `json:"unrepresented_bytes"`
	Sources            []OverflowSource `json:"sources,omitempty"`
}

// OverflowSource makes completely unrepresented records discoverable without
// trying to fit one manifest row per item inside the same bounded projection.
type OverflowSource struct {
	Source        string   `json:"source"`
	Priority      Priority `json:"priority"`
	FirstID       string   `json:"first_id"`
	LastID        string   `json:"last_id"`
	Items         int      `json:"items"`
	Bytes         int      `json:"bytes"`
	ContentDigest string   `json:"content_digest"`
	Open          string   `json:"open"`
}

// Prepared is a rendered projection with acknowledgements for exactly the
// records represented by a full body or stable retrieval header.
type Prepared struct {
	Text   string
	Report Report
	ack    []func() error
}

// Commit acknowledges only records represented in Text. It must be called only
// after successful delivery. Store adapters are responsible for idempotence.
func (p Prepared) Commit() error {
	for _, ack := range p.ack {
		if ack != nil {
			if err := ack(); err != nil {
				return err
			}
		}
	}
	return nil
}

// UTF8Prefix returns the longest valid-UTF-8 prefix no larger than max bytes.
// Invalid input is refused rather than repaired because a digest/reference must
// identify the exact authoritative bytes.
func UTF8Prefix(s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("admission: input is not valid UTF-8")
	}
	if max <= 0 {
		return "", nil
	}
	if len(s) <= max {
		return s, nil
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max], nil
}

type candidate struct {
	item Item
	mode int // 0 unrepresented, 1 stable reference header, 2 full body
	full string
	head string
}

// Render selects and renders prepared records under one deterministic byte
// ceiling. Urgent records receive a representation before any body is admitted.
func Render(items []Item, opt Options) (Prepared, error) {
	budget := opt.BudgetBytes
	if budget <= 0 {
		budget = DefaultTurnBytes
	}
	preview := opt.PreviewBytes
	if preview <= 0 {
		preview = DefaultPreviewBytes
	}

	candidates := make([]candidate, len(items))
	for i, item := range items {
		item.Source = strings.TrimSpace(item.Source)
		item.ID = strings.TrimSpace(item.ID)
		item.ArtifactRef = strings.TrimSpace(item.ArtifactRef)
		item.OverflowRef = strings.TrimSpace(item.OverflowRef)
		if item.Source == "" || item.ID == "" {
			return Prepared{}, fmt.Errorf("admission: item %d needs source and id", i)
		}
		for _, field := range []struct{ name, value string }{
			{"source", item.Source}, {"id", item.ID}, {"from", item.From},
			{"to", item.To}, {"topic", item.Topic}, {"body", item.Body},
			{"artifact_ref", item.ArtifactRef}, {"overflow_ref", item.OverflowRef},
		} {
			if !utf8.ValidString(field.value) {
				return Prepared{}, fmt.Errorf("admission: item %s %q is not valid UTF-8", field.name, item.ID)
			}
		}
		if item.Priority < PriorityUrgent || item.Priority > PriorityInformational {
			item.Priority = PriorityInformational
		}
		candidates[i] = candidate{item: item}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].item.Priority < candidates[j].item.Priority
	})
	for i := range candidates {
		candidates[i].full = renderFull(candidates[i].item)
		if candidates[i].item.ArtifactRef != "" {
			candidates[i].head = renderReference(candidates[i].item)
		}
	}

	// Reserve a representation for every urgent record before spending a byte
	// on bodies. This is the priority invariant that prevents a large routine
	// message from hiding a later BLOCKED/CONFLICT/security/ownership header.
	for i := range candidates {
		c := &candidates[i]
		if c.item.Priority != PriorityUrgent {
			continue
		}
		switch {
		case c.head != "":
			c.mode = 1
		case len(c.item.Body) <= preview:
			c.mode = 2
		default:
			return Prepared{}, fmt.Errorf("admission: urgent item %s:%s has no stable retrieval reference", c.item.Source, c.item.ID)
		}
	}
	if text, _, err := compose(candidates); err != nil || len(text) > budget {
		return Prepared{}, fmt.Errorf("admission: %d-byte budget cannot represent every urgent header", budget)
	}

	// Then spend remaining bytes in priority order. A short body is preferred;
	// a large body receives a stable reference header; an item that fits neither
	// stays unread and is counted by the overflow manifest.
	for i := range candidates {
		c := &candidates[i]
		want := 0
		switch {
		case len(c.item.Body) <= preview:
			want = 2
		case c.head != "":
			want = 1
		}
		if want <= c.mode {
			continue
		}
		old := c.mode
		c.mode = want
		text, _, err := compose(candidates)
		if err != nil || len(text) > budget {
			c.mode = old
		}
	}

	text, report, err := compose(candidates)
	if err != nil {
		return Prepared{}, err
	}
	if len(text) > budget {
		return Prepared{}, fmt.Errorf("admission: internal rendering exceeded %d-byte budget", budget)
	}
	for _, c := range candidates {
		if c.mode == 0 && c.item.OverflowRef == "" {
			return Prepared{}, fmt.Errorf("admission: unrepresented item %s:%s has no source retrieval reference", c.item.Source, c.item.ID)
		}
	}
	prepared := Prepared{Text: text, Report: report}
	for _, c := range candidates {
		if c.mode > 0 && c.item.Acknowledge != nil {
			prepared.ack = append(prepared.ack, c.item.Acknowledge)
		}
	}
	return prepared, nil
}

func compose(candidates []candidate) (string, Report, error) {
	report := Report{SchemaVersion: OverflowSchemaVersion, InputItems: len(candidates)}
	var b strings.Builder
	if len(candidates) > 0 {
		b.WriteString("## Coordination input\n\n")
	}
	for _, c := range candidates {
		report.InputBytes += len(c.item.Body)
		switch c.mode {
		case 2:
			b.WriteString(c.full)
			report.AdmittedItems++
			report.FullItems++
		case 1:
			b.WriteString(c.head)
			report.AdmittedItems++
			report.ReferencedItems++
			report.OmittedItems++
			report.OmittedBytes += len(c.item.Body)
		default:
			report.OmittedItems++
			report.OmittedBytes += len(c.item.Body)
			report.UnrepresentedItems++
			report.UnrepresentedBytes += len(c.item.Body)
		}
	}
	report.ContentDigest = digestCandidates(candidates)
	if report.OmittedItems > 0 {
		overflow := Overflow{
			SchemaVersion: OverflowSchemaVersion, ContentDigest: report.ContentDigest,
			OmittedItems: report.OmittedItems, OmittedBytes: report.OmittedBytes,
			ReferencedItems:    report.ReferencedItems,
			UnrepresentedItems: report.UnrepresentedItems,
			UnrepresentedBytes: report.UnrepresentedBytes,
			Sources:            overflowSources(candidates),
		}
		encoded, err := json.Marshal(overflow)
		if err != nil {
			return "", Report{}, err
		}
		b.WriteString("\n## Coordination overflow\n\n")
		b.Write(encoded)
		b.WriteByte('\n')
	}
	report.RenderedBytes = b.Len()
	return b.String(), report, nil
}

func overflowSources(candidates []candidate) []OverflowSource {
	type group struct {
		out   OverflowSource
		items []candidate
	}
	var groups []group
	for _, c := range candidates {
		if c.mode != 0 {
			continue
		}
		at := -1
		for i := range groups {
			if groups[i].out.Source == c.item.Source && groups[i].out.Open == c.item.OverflowRef {
				at = i
				break
			}
		}
		if at < 0 {
			groups = append(groups, group{out: OverflowSource{
				Source: c.item.Source, Priority: c.item.Priority, FirstID: c.item.ID,
				LastID: c.item.ID, Open: c.item.OverflowRef,
			}})
			at = len(groups) - 1
		}
		g := &groups[at]
		g.out.Items++
		g.out.Bytes += len(c.item.Body)
		g.out.LastID = c.item.ID
		if c.item.Priority < g.out.Priority {
			g.out.Priority = c.item.Priority
		}
		g.items = append(g.items, c)
	}
	out := make([]OverflowSource, len(groups))
	for i := range groups {
		groups[i].out.ContentDigest = digestCandidates(groups[i].items)
		out[i] = groups[i].out
	}
	return out
}

func renderFull(item Item) string {
	return renderPrefix(item) + " — " + item.Body + "\n"
}

func renderReference(item Item) string {
	return fmt.Sprintf("%s — body=%dB digest=%s open=%s\n",
		renderPrefix(item), len(item.Body), digestText(item.Body), strconv.Quote(item.ArtifactRef))
}

func renderPrefix(item Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- [P%d] %s:%s", item.Priority, item.Source, item.ID)
	if item.Topic != "" {
		fmt.Fprintf(&b, " topic=%s", strconv.Quote(item.Topic))
	}
	if item.From != "" {
		fmt.Fprintf(&b, " from=%s", strconv.Quote(item.From))
	}
	if item.To != "" {
		fmt.Fprintf(&b, " to=%s", strconv.Quote(item.To))
	}
	return b.String()
}

func digestText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestCandidates(candidates []candidate) string {
	h := sha256.New()
	for _, c := range candidates {
		fields := []string{c.item.Source, c.item.ID, strconv.FormatInt(c.item.Sequence, 10),
			c.item.From, c.item.To, c.item.Topic, strconv.Itoa(int(c.item.Priority)),
			c.item.Body, c.item.ArtifactRef, c.item.OverflowRef}
		for _, field := range fields {
			fmt.Fprintf(h, "%d:", len(field))
			h.Write([]byte(field))
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
