package kb

import (
	"strings"
	"testing"
)

func diagCorpus() []*Page {
	return []*Page{
		{
			Slug: "podman-rootless-uid", Type: "gotcha",
			Title:       "Rootless podman needs a subuid range",
			Description: "Container fails to start when /etc/subuid has no entry for the user",
			Tags:        []string{"podman", "container"},
			Body:        "Check the subuid mapping before blaming the image.",
			Status:      StatusValidated,
		},
		{
			Slug: "go-test-hangs-tty", Type: "lesson",
			Title:       "Go tests that drive a pty hang without a controlling terminal",
			Description: "The readline goroutine blocks on read and the suite runs to its timeout",
			Tags:        []string{"golang", "testing"},
			Body:        "Run the rest with -short. The deadlock is structural.",
		},
		{
			Slug: "linux-only-note", Type: "fact",
			Title:       "Cgroup accounting differs on this kernel",
			Description: "Only relevant on linux hosts",
			Tags:        []string{"linux"},
			Scope:       &Scope{OS: "linux"},
		},
	}
}

func TestDiagnoseReportsPerTermDocumentFrequency(t *testing.T) {
	rep := Diagnose(diagCorpus(), Query{Terms: Terms("podman subuid"), OS: "darwin"})

	if rep.Total != 3 {
		t.Errorf("Total = %d, want 3", rep.Total)
	}
	// The linux-scoped page is filtered out on darwin, and the report must
	// describe the set that was ranked, not the whole store.
	if rep.Eligible != 2 {
		t.Errorf("Eligible = %d, want 2 (linux page filtered)", rep.Eligible)
	}

	byTerm := map[string]TermReport{}
	for _, tr := range rep.Terms {
		byTerm[tr.Term] = tr
	}

	podman := byTerm["podman"]
	if podman.Pages != 1 || podman.CuePages != 1 {
		t.Errorf("podman = %d pages / %d cue, want 1/1", podman.Pages, podman.CuePages)
	}
	// "subuid" is in the description of one page and the body of the same one.
	subuid := byTerm["subuid"]
	if subuid.Pages != 1 {
		t.Errorf("subuid Pages = %d, want 1", subuid.Pages)
	}
	if !rep.Matched() {
		t.Error("Matched() = false, want true — every term is carried by some page")
	}
}

func TestDiagnoseSeparatesBodyOnlyTermsFromCueTerms(t *testing.T) {
	// "blaming" appears only in a body, never on a routing surface. That is
	// exactly why such a page ranks weakly, so the report has to say it.
	rep := Diagnose(diagCorpus(), Query{Terms: Terms("blaming"), OS: "darwin"})
	if len(rep.Terms) != 1 {
		t.Fatalf("Terms = %d, want 1", len(rep.Terms))
	}
	got := rep.Terms[0]
	if got.Pages != 1 {
		t.Errorf("Pages = %d, want 1", got.Pages)
	}
	if got.CuePages != 0 {
		t.Errorf("CuePages = %d, want 0 — the term is body-only", got.CuePages)
	}
	if !strings.Contains(rep.Text(), "none in a title/description/tag") {
		t.Errorf("Text() should flag the body-only term:\n%s", rep.Text())
	}
}

func TestDiagnoseReportsUnknownTermsAndOffersVocabulary(t *testing.T) {
	rep := Diagnose(diagCorpus(), Query{Terms: Terms("zygomorphic pelagic"), OS: "darwin"})

	for _, tr := range rep.Terms {
		if tr.Pages != 0 {
			t.Errorf("%q Pages = %d, want 0", tr.Term, tr.Pages)
		}
	}
	if rep.Matched() {
		t.Error("Matched() = true, want false")
	}
	if len(rep.Vocab) == 0 {
		t.Fatal("Vocab is empty — an unmatched query has nothing to reformulate from")
	}
	// The vocabulary must come from the eligible set only: suggesting a term
	// that only exists on a filtered-out page sends the reader after a page
	// this query can never return.
	for _, v := range rep.Vocab {
		if v == "cgroup" || v == "kernel" {
			t.Errorf("Vocab leaked %q from the out-of-scope linux page", v)
		}
	}
	txt := rep.Text()
	if !strings.Contains(txt, "no page uses it") {
		t.Errorf("Text() should say the term is unknown:\n%s", txt)
	}
	if !strings.Contains(txt, "this kb talks about") {
		t.Errorf("Text() should offer the corpus vocabulary:\n%s", txt)
	}
}

func TestDiagnoseSuggestsMorphologicalNeighbours(t *testing.T) {
	// Trigrams buy morphology and nothing else. "container" is a cue term;
	// "containers" is not in the corpus and should find it.
	rep := Diagnose(diagCorpus(), Query{Terms: Terms("containers"), OS: "darwin"})
	if len(rep.Terms) != 1 {
		t.Fatalf("Terms = %d, want 1", len(rep.Terms))
	}
	got := rep.Terms[0]
	if got.Pages != 0 {
		t.Fatalf("Pages = %d, want 0", got.Pages)
	}
	var found bool
	for _, n := range got.Near {
		if n == "container" {
			found = true
		}
	}
	if !found {
		t.Errorf("Near = %v, want it to contain %q", got.Near, "container")
	}
}

func TestDiagnoseDoesNotInventSynonyms(t *testing.T) {
	// The measured boundary: trigrams cannot cross a synonymy gap. "stall"
	// shares no characters with "hang"/"block", so Near must stay empty
	// rather than manufacture a suggestion. Meaning is the reader's job.
	rep := Diagnose(diagCorpus(), Query{Terms: Terms("stall"), OS: "darwin"})
	if len(rep.Terms) != 1 {
		t.Fatalf("Terms = %d, want 1", len(rep.Terms))
	}
	if n := rep.Terms[0].Near; len(n) != 0 {
		t.Errorf("Near = %v, want empty — trigrams must not claim synonymy", n)
	}
}

func TestDiagnoseReportsAnUnusableQuery(t *testing.T) {
	// Every word a stopword: the query never reached the ranker at all, which
	// is a different failure from "the corpus lacks this".
	rep := Diagnose(diagCorpus(), Query{Terms: Terms("how do I use the one that was"), OS: "darwin"})
	if len(rep.Terms) != 0 {
		t.Fatalf("Terms = %v, want none", rep.Terms)
	}
	if !strings.Contains(rep.Text(), "no usable query terms") {
		t.Errorf("Text() should name the real failure:\n%s", rep.Text())
	}
}

func TestDiagnoseReportsAnEmptyEligibleSet(t *testing.T) {
	// A store with pages, none of them reachable from here. Blaming the query
	// would be wrong.
	rep := Diagnose(diagCorpus(), Query{Terms: Terms("podman"), Tags: []string{"nonexistent"}})
	if rep.Eligible != 0 {
		t.Fatalf("Eligible = %d, want 0", rep.Eligible)
	}
	if !strings.Contains(rep.Text(), "filtered out") {
		t.Errorf("Text() should blame the filter, not the query:\n%s", rep.Text())
	}
}

func TestDiagnoseAgreesWithSearchOnEligibility(t *testing.T) {
	// The report is worthless if it describes a different set than the one
	// Search ranked. Any future divergence in the filters fails here.
	pages := diagCorpus()
	for _, q := range []Query{
		{Terms: Terms("podman"), OS: "darwin"},
		{Terms: Terms("podman"), OS: "linux"},
		{Terms: Terms("podman"), Tags: []string{"podman"}},
	} {
		rep := Diagnose(pages, q)
		if got := len(eligible(pages, q)); rep.Eligible != got {
			t.Errorf("q=%+v: Report.Eligible = %d, Search saw %d", q, rep.Eligible, got)
		}
	}
}

func TestDiagnoseIsStable(t *testing.T) {
	// Two identical calls must render identically — an agent diffing advice
	// across runs should see change only when the corpus changed.
	q := Query{Terms: Terms("zygomorphic"), OS: "darwin"}
	first := Diagnose(diagCorpus(), q).Text()
	for range 5 {
		if got := Diagnose(diagCorpus(), q).Text(); got != first {
			t.Fatalf("unstable output:\n%s\nvs\n%s", first, got)
		}
	}
}

// TestDiagnoseVocabularyPrefersCuratedTags pins the vocabulary source. Ranking
// the whole routing surface by document frequency puts BOILERPLATE first — a
// description template repeated across pages outranks every real subject — and
// a reader reformulating toward "generated" has been actively misled.
func TestDiagnoseVocabularyPrefersCuratedTags(t *testing.T) {
	const boilerplate = "Automatically transcribed placeholder note, annotated verbatim from upstream provenance"
	topics := []string{"podman", "kubernetes", "systemd", "openssl", "postgres", "nginx", "redis", "kafka"}
	var pages []*Page
	for i, topic := range topics {
		for _, variant := range []string{"alpha", "beta"} {
			pages = append(pages, &Page{
				Slug:  topic + "-" + variant,
				Type:  "lesson",
				Title: "Note " + variant + " number " + string(rune('a'+i)),
				// Every page shares this, so its words reach df=16 while each
				// real subject only reaches df=2.
				Description: boilerplate,
				Tags:        []string{topic},
			})
		}
	}

	rep := Diagnose(pages, Query{Terms: Terms("zygomorphic")})
	if len(rep.Vocab) == 0 {
		t.Fatal("Vocab is empty")
	}
	got := strings.Join(rep.Vocab, " ")
	for _, noise := range strings.Fields(strings.ToLower(boilerplate)) {
		if len(noise) < 3 {
			continue
		}
		if strings.Contains(got, strings.Trim(noise, ",")) {
			t.Errorf("Vocab leaked boilerplate %q: %v", noise, rep.Vocab)
		}
	}
	for _, topic := range topics {
		if !strings.Contains(got, topic) {
			t.Errorf("Vocab is missing the real subject %q: %v", topic, rep.Vocab)
		}
	}
}

// TestDiagnoseVocabularyFallsBackWhenUntagged covers a corpus that carries no
// tags at all: a noisy vocabulary still beats none.
func TestDiagnoseVocabularyFallsBackWhenUntagged(t *testing.T) {
	var pages []*Page
	for _, w := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota"} {
		pages = append(pages,
			&Page{Slug: w + "-one", Title: "The " + w + " runbook", Type: "runbook"},
			&Page{Slug: w + "-two", Title: "More " + w + " notes", Type: "runbook"},
		)
	}
	rep := Diagnose(pages, Query{Terms: Terms("zygomorphic")})
	if len(rep.Vocab) == 0 {
		t.Fatal("Vocab is empty on an untagged corpus — the fallback did not fire")
	}
}

// TestNearMinDiceSeparatesMorphologyFromCoincidence pins the constant. The
// threshold is the whole difference between a did-you-mean list and a synonym
// generator, and it was set from these measurements — so they are the test.
func TestNearMinDiceSeparatesMorphologyFromCoincidence(t *testing.T) {
	morphology := [][2]string{
		{"container", "containers"}, {"conform", "conformance"},
		{"test", "testing"}, {"config", "configuration"},
		{"retry", "retries"}, {"resolve", "resolution"},
	}
	coincidence := [][2]string{
		{"wait", "trait"}, {"hang", "range"},
		{"stall", "start"}, {"cache", "catch"},
	}
	for _, p := range morphology {
		if d := dice(trigrams(p[0]), trigrams(p[1])); d < nearMinDice {
			t.Errorf("%s/%s dice=%.3f < %.2f — a real variant would be dropped", p[0], p[1], d, nearMinDice)
		}
	}
	for _, p := range coincidence {
		if d := dice(trigrams(p[0]), trigrams(p[1])); d >= nearMinDice {
			t.Errorf("%s/%s dice=%.3f >= %.2f — an invented synonym would be offered", p[0], p[1], d, nearMinDice)
		}
	}
	// The prefix gate is not redundant with the dice gate. "stalled" and
	// "install" both contain "stall", which rates them 0.600 — higher than
	// every genuine variant except container/containers. Only the shared-stem
	// rule rejects them, and this was a live false suggestion before it existed.
	if d := dice(trigrams("stalled"), trigrams("install")); d < nearMinDice {
		t.Fatalf("premise changed: stalled/install dice=%.3f, the prefix gate is no longer load-bearing", d)
	}
	if sharesStem("stalled", "install") {
		t.Error("sharesStem(stalled, install) = true — substring overlap is not morphology")
	}
	for _, p := range morphology {
		if !sharesStem(p[0], p[1]) {
			t.Errorf("sharesStem(%s, %s) = false — a real variant would be dropped", p[0], p[1])
		}
	}
	if !sharesStem("log", "logs") {
		t.Error("sharesStem(log, logs) = false — a short stem must still reach its plural")
	}
}
