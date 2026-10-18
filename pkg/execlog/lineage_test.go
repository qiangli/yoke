package execlog

import "testing"

// TestAppendRecordsParentEpisode: a record for this process's own episode
// carries BASHY_PARENT_EPISODE; a record for any other episode does not.
func TestAppendRecordsParentEpisode(t *testing.T) {
	t.Setenv("BASHY_EPISODE", "ep-child")
	t.Setenv("BASHY_PARENT_EPISODE", "ep-parent")
	root := t.TempDir()
	w := Open(root)
	body := Scrub(nil, []string{"true"}, "/w", TemplateOpts{})
	for _, ep := range []string{"ep-child", "ep-other"} {
		if err := w.Append(Record{Cmd: "true", Observed: true}, body, ep); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	for ep, want := range map[string]string{"ep-child": "ep-parent", "ep-other": ""} {
		recs, _, err := Read(root, Query{Episode: ep})
		if err != nil || len(recs) != 1 {
			t.Fatalf("read %s: %d records, err=%v", ep, len(recs), err)
		}
		if recs[0].ParentEpisode != want {
			t.Errorf("%s parent_episode = %q, want %q", ep, recs[0].ParentEpisode, want)
		}
	}
}
