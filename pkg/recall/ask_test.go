package recall

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/kb"
)

func askGolden(t *testing.T, name string, value any) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, want) {
		t.Fatalf("%s\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}
func TestAskEnvelopeGoldens(t *testing.T) {
	st := askState{Question: "alpha beta", Round: 1, MaxRounds: 4, MinCoverage: .6, Evidence: []AskEvidence{{Ref: "kb:one", Ring: RingHost, Text: "alpha beta", Path: []string{"kb:one", "file:///one"}}}}
	step := makeAskStep(st, "session", nil)
	askGolden(t, "kb-ask-step.json", step)
	final := finishAsk(&st, "session", func(ref, ring string) kb.ShowEnvelope { return kb.ShowEnvelope{Ref: ref, Outcome: "found"} }, nil)
	askGolden(t, "kb-ask-final.json", final)
}
func TestAskAbstainsOnUnanswerableQuestion(t *testing.T) {
	st := askState{Question: "unanswerable", Round: 1, MaxRounds: 4, MinCoverage: .6, Evidence: []AskEvidence{}}
	got := finishAsk(&st, "session", func(string, string) kb.ShowEnvelope { t.Fatal("unexpected show"); return kb.ShowEnvelope{} }, nil)
	if !got.Abstained || !slicesHas(got.Reasons, "insufficient-coverage") || len(got.Citations) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestAskCommandSessionAndUnanswerableFinal(t *testing.T) {
	root := isolateRecallStores(t)
	d := askDeps{stateDir: filepath.Join(root, "ask"), show: func(string, string) kb.ShowEnvelope {
		t.Fatal("an empty answer must not cite")
		return kb.ShowEnvelope{}
	}}
	first := newAskCmd(d)
	var out bytes.Buffer
	first.SetOut(&out)
	first.SetArgs([]string{"unanswerable fixture", "--rounds", "1", "--json"})
	if err := first.Execute(); err != nil {
		t.Fatal(err)
	}
	var step AskStep
	if err := json.Unmarshal(out.Bytes(), &step); err != nil {
		t.Fatal(err)
	}
	if step.Round != 1 || len(step.Evidence) != 0 || step.Coverage != 0 {
		t.Fatalf("%+v", step)
	}
	second := newAskCmd(d)
	out.Reset()
	second.SetOut(&out)
	second.SetArgs([]string{"--session", step.Session, "--final", "--json"})
	if err := second.Execute(); err != nil {
		t.Fatal(err)
	}
	var final AskFinal
	if err := json.Unmarshal(out.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	if !final.Abstained || !slicesHas(final.Reasons, "insufficient-coverage") {
		t.Fatalf("%+v", final)
	}
}
func TestAskSupportedContradictoryExpiredDenied(t *testing.T) {
	root := isolateRecallStores(t)
	host := kb.DefaultDir()
	store := kb.Open(host)
	for _, p := range []*kb.Page{
		{Slug: "alpha", Type: kb.TypeLesson, Title: "alpha", Description: "alpha beta", Status: kb.StatusValidated},
		{Slug: "beta", Type: kb.TypeLesson, Title: "beta", Description: "alpha beta", Status: kb.StatusValidated},
	} {
		if err := store.Write(p, "add"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("supported citations open through show", func(t *testing.T) {
		d := askDeps{readers: []Reader{HostRing{Store: store, Path: host}}, show: showAskRef}
		st := askState{Question: "alpha beta", MaxRounds: 4, MinCoverage: .6, Evidence: []AskEvidence{}, Blocking: map[string]string{}}
		step, err := askRound(&st, "session", st.Question, d)
		if err != nil {
			t.Fatal(err)
		}
		if len(step.Evidence) == 0 {
			t.Fatal("no evidence")
		}
		got := finishAsk(&st, "session", d.show, d.readers)
		if got.Abstained || len(got.Citations) == 0 {
			t.Fatalf("%+v", got)
		}
		for _, cite := range got.Citations {
			if len(cite.Path) == 0 || showAskRef(cite.Ref, RingHost).Outcome != "found" {
				t.Fatalf("citation cannot open: %+v", cite)
			}
		}
	})
	t.Run("contradictory", func(t *testing.T) {
		rel := `{"id":"r1","op":"link","target":"kb:alpha","relation":"contradicts","dst":"kb:beta"}` + "\n"
		if err := os.WriteFile(kb.RelationPath(host), []byte(rel), 0600); err != nil {
			t.Fatal(err)
		}
		st := askState{Question: "alpha beta", MaxRounds: 4, MinCoverage: .6, Evidence: []AskEvidence{{Ref: "kb:alpha", Ring: RingHost, Text: "alpha beta"}, {Ref: "kb:beta", Ring: RingHost, Text: "alpha beta"}}}
		readers := []Reader{RelationRing{RingName: RingHost, Path: host}}
		if pairs := askContradictions(st.Evidence, readers); len(pairs) != 1 || pairs[0].Relation != "contradicts" {
			t.Fatalf("pairs=%+v", pairs)
		}
		got := finishAsk(&st, "session", showAskRef, readers)
		if !got.Abstained || !slicesHas(got.Reasons, "contradiction") || !reflect.DeepEqual(got.BlockingRefs, []string{"kb:alpha", "kb:beta"}) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("expired", func(t *testing.T) {
		st := askState{Question: "alpha", MinCoverage: .6, Evidence: []AskEvidence{{Ref: "kb:alpha", Ring: RingHost, Text: "alpha"}}}
		got := finishAsk(&st, "session", func(ref, ring string) kb.ShowEnvelope { return kb.ShowEnvelope{Ref: ref, Outcome: "expired"} }, nil)
		if !got.Abstained || !slicesHas(got.Reasons, "expired") || !reflect.DeepEqual(got.BlockingRefs, []string{"kb:alpha"}) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("denied ref is hidden", func(t *testing.T) {
		st := askState{Question: "alpha secret", MaxRounds: 4, MinCoverage: .6, Evidence: []AskEvidence{}, Blocking: map[string]string{}}
		d := askDeps{readers: []Reader{staticReader{ring: RingHost, hits: []Hit{{Ring: RingHost, ID: "kb:hidden", Form: kb.FormPage, Cue: "alpha secret private body", Score: 1}}}}, show: func(ref, ring string) kb.ShowEnvelope { return kb.ShowEnvelope{Ref: ref, Outcome: "denied"} }}
		step, err := askRound(&st, "session", st.Question, d)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(step)
		if strings.Contains(string(b), "private body") || len(step.Evidence) != 0 || len(step.SuggestedFollowUps) == 0 {
			t.Fatalf("step leaked or omitted followup: %s", b)
		}
		got := finishAsk(&st, "session", d.show, d.readers)
		if !got.Abstained || !slicesHas(got.Reasons, "denied") || !reflect.DeepEqual(got.BlockingRefs, []string{"kb:hidden"}) {
			t.Fatalf("%+v", got)
		}
	})
	_ = root
}
func slicesHas(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

type askStubModel struct{ calls int }

func (m *askStubModel) Complete(_ context.Context, _ string) (string, error) {
	m.calls++
	return "beta", nil
}
func TestAskLocalReasonerDrivesSameRounds(t *testing.T) {
	root := isolateRecallStores(t)
	model := &askStubModel{}
	reader := askQueryReader{}
	d := askDeps{readers: []Reader{reader}, stateDir: filepath.Join(root, "ask"), complete: model, show: func(ref, ring string) kb.ShowEnvelope { return kb.ShowEnvelope{Ref: ref, Outcome: "found"} }}
	cmd := newAskCmd(d)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"alpha beta", "--reasoner", "local", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var final AskFinal
	if err := json.Unmarshal(out.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || final.Abstained || final.Coverage != 1 || len(final.Citations) != 2 {
		t.Fatalf("calls=%d final=%+v", model.calls, final)
	}
	st, err := loadAsk(d.stateDir, final.Session)
	if err != nil || !st.Closed || st.Round != 2 {
		t.Fatalf("state=%+v err=%v", st, err)
	}
}

type askQueryReader struct{}

func (askQueryReader) Ring() string    { return RingHost }
func (askQueryReader) Forms() []string { return []string{kb.FormPage} }
func (askQueryReader) Recall(q Query) ([]Hit, error) {
	if q.Text == "beta" {
		return []Hit{{Ring: RingHost, ID: "kb:beta", Form: kb.FormPage, Cue: "beta", Score: 1}}, nil
	}
	return []Hit{{Ring: RingHost, ID: "kb:alpha", Form: kb.FormPage, Cue: "alpha", Score: 1}}, nil
}
