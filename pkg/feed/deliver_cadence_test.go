package feed

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// wall returns 2026-10-12 (a Monday) at minute-of-day min, local zone UTC.
func wall(min int) time.Time {
	return time.Date(2026, 10, 12, min/60, min%60, 0, 0, time.UTC)
}

func cand(id, author string, createdMin int) Candidate {
	return Candidate{
		ID:       id,
		Author:   author,
		Headline: "Head " + id,
		Link:     "https://example.com/" + id,
		Created:  wall(createdMin),
	}
}

func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestDefaultDeliveryPrefs(t *testing.T) {
	got := DefaultDeliveryPrefs()
	want := DeliveryPrefs{
		Interval:   4 * time.Hour,
		MaxItems:   10,
		QuietStart: 22 * 60,
		QuietEnd:   7 * 60,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultDeliveryPrefs() = %+v, want %+v", got, want)
	}
}

func TestPlanDelivery(t *testing.T) {
	noon := wall(12 * 60)
	ranked := []Candidate{
		cand("v-old", "vip", 8*60),
		cand("n-new", "norm", 15*60),
		cand("v-new", "vip", 14*60),
		cand("n-old", "norm", 9*60),
	}
	overflow := func(n int) []Candidate {
		cs := make([]Candidate, n)
		for i := range cs {
			cs[i] = cand(cname(i), "norm", (n-i)*60)
		}
		return cs
	}
	wantIDs := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = cname(i)
		}
		return out
	}

	cases := []struct {
		name        string
		now, last   time.Time
		pending     []Candidate
		readIDs     map[string]bool
		passNilRead bool
		prefs       DeliveryPrefs
		inboxURL    string
		wantSend    bool
		wantIDs     []string
		wantMore    int
		wantLink    string
	}{
		{
			name:     "no pending never sends",
			now:      noon,
			prefs:    DefaultDeliveryPrefs(),
			wantSend: false,
		},
		{
			name:     "all read never sends",
			now:      noon,
			pending:  []Candidate{cand("a", "norm", 9*60), cand("b", "norm", 10*60)},
			readIDs:  map[string]bool{"a": true, "b": true},
			prefs:    DefaultDeliveryPrefs(),
			wantSend: false,
		},
		{
			name:    "off never sends even for immediate authors with no history",
			now:     noon,
			pending: []Candidate{cand("a", "vip", 9*60)},
			prefs: func() DeliveryPrefs {
				p := DefaultDeliveryPrefs()
				p.Immediate = []string{"vip"}
				p.Off = true
				return p
			}(),
			wantSend: false,
		},
		{
			name:        "nil read treats everything as unread",
			now:         noon,
			pending:     []Candidate{cand("a", "norm", 9*60), cand("b", "norm", 10*60)},
			readIDs:     map[string]bool{"a": true},
			passNilRead: true,
			prefs:       DefaultDeliveryPrefs(),
			wantSend:    true,
			wantIDs:     []string{"b", "a"},
		},
		{
			name:     "first batch with last zero ranks immediate first then newest",
			now:      noon,
			pending:  ranked,
			prefs:    func() DeliveryPrefs { p := DefaultDeliveryPrefs(); p.Immediate = []string{"vip"}; return p }(),
			wantSend: true,
			wantIDs:  []string{"v-new", "v-old", "n-new", "n-old"},
		},
		{
			name:     "interval not elapsed holds regular items",
			now:      noon,
			last:     wall(11 * 60),
			pending:  []Candidate{cand("a", "norm", 11*60+30)},
			prefs:    DefaultDeliveryPrefs(),
			wantSend: false,
		},
		{
			name:     "interval elapsed exactly sends",
			now:      noon,
			last:     wall(8 * 60),
			pending:  []Candidate{cand("a", "norm", 9*60)},
			prefs:    DefaultDeliveryPrefs(),
			wantSend: true,
			wantIDs:  []string{"a"},
		},
		{
			name:     "immediate author bypasses the interval",
			now:      noon,
			last:     wall(11 * 60),
			pending:  []Candidate{cand("hold", "norm", 11*60+30), cand("now", "vip", 11*60+45)},
			prefs:    func() DeliveryPrefs { p := DefaultDeliveryPrefs(); p.Immediate = []string{"vip"}; return p }(),
			wantSend: true,
			wantIDs:  []string{"now"},
		},
		{
			name:     "quiet hours hold regular items even when the interval is due",
			now:      wall(23*60 + 30),
			pending:  []Candidate{cand("a", "norm", 22*60)},
			prefs:    DefaultDeliveryPrefs(),
			wantSend: false,
		},
		{
			name:     "quiet hours pass immediate authors only, bypassing the interval",
			now:      wall(3 * 60),
			last:     wall(2*60 + 55),
			pending:  []Candidate{cand("hold", "norm", 2*60+58), cand("now", "vip", 2*60+59)},
			prefs:    func() DeliveryPrefs { p := DefaultDeliveryPrefs(); p.Immediate = []string{"vip"}; return p }(),
			wantSend: true,
			wantIDs:  []string{"now"},
		},
		{
			name:     "read item dropped before the count so it never inflates More",
			now:      noon,
			pending:  []Candidate{cand("read", "norm", 15*60), cand("a", "norm", 12*60), cand("b", "norm", 11*60), cand("c", "norm", 10*60)},
			readIDs:  map[string]bool{"read": true},
			prefs:    func() DeliveryPrefs { p := DefaultDeliveryPrefs(); p.MaxItems = 2; return p }(),
			inboxURL: "https://inbox.example/x",
			wantSend: true,
			wantIDs:  []string{"a", "b"},
			wantMore: 1,
			wantLink: "https://inbox.example/x",
		},
		{
			name:     "overflow of exactly MaxItems+1 with the default cap of 10",
			now:      noon,
			pending:  overflow(11),
			prefs:    DefaultDeliveryPrefs(),
			inboxURL: "https://inbox.example/x",
			wantSend: true,
			wantIDs:  wantIDs(10),
			wantMore: 1,
			wantLink: "https://inbox.example/x",
		},
		{
			name:     "larger overflow collapses beyond a small cap",
			now:      noon,
			pending:  overflow(5),
			prefs:    func() DeliveryPrefs { p := DefaultDeliveryPrefs(); p.MaxItems = 2; return p }(),
			inboxURL: "https://inbox.example/x",
			wantSend: true,
			wantIDs:  wantIDs(2),
			wantMore: 3,
			wantLink: "https://inbox.example/x",
		},
		{
			name:     "within MaxItems there is no overflow line",
			now:      noon,
			pending:  overflow(3),
			prefs:    DefaultDeliveryPrefs(),
			inboxURL: "https://inbox.example/x",
			wantSend: true,
			wantIDs:  wantIDs(3),
		},
		{
			name:     "zero prefs behave as defaults: 30 minutes is not the interval",
			now:      noon,
			last:     wall(11*60 + 30),
			pending:  []Candidate{cand("a", "norm", 11*60+40)},
			prefs:    DeliveryPrefs{},
			wantSend: false,
		},
		{
			name:     "zero prefs behave as defaults: default quiet window applies",
			now:      wall(23*60 + 30),
			pending:  []Candidate{cand("a", "norm", 22*60)},
			prefs:    DeliveryPrefs{},
			wantSend: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var read func(id string) bool
			if !tc.passNilRead {
				read = func(id string) bool { return tc.readIDs[id] }
			}
			batch, ok := PlanDelivery(tc.now, tc.last, tc.pending, read, tc.prefs, tc.inboxURL)
			if ok != tc.wantSend {
				t.Fatalf("send = %v, want %v", ok, tc.wantSend)
			}
			if !tc.wantSend {
				if len(batch.Items) != 0 || batch.More != 0 || batch.MoreLink != "" {
					t.Fatalf("held batch = %+v, want zero Batch", batch)
				}
				return
			}
			if got := ids(batch.Items); !reflect.DeepEqual(got, tc.wantIDs) {
				t.Fatalf("items = %v, want %v", got, tc.wantIDs)
			}
			if batch.More != tc.wantMore {
				t.Fatalf("More = %d, want %d", batch.More, tc.wantMore)
			}
			if batch.MoreLink != tc.wantLink {
				t.Fatalf("MoreLink = %q, want %q", batch.MoreLink, tc.wantLink)
			}
		})
	}

	t.Run("pending slice is not mutated", func(t *testing.T) {
		pending := append([]Candidate(nil), ranked...)
		if _, ok := PlanDelivery(noon, time.Time{}, pending, nil, DefaultDeliveryPrefs(), ""); !ok {
			t.Fatal("send = false, want true")
		}
		if !reflect.DeepEqual(pending, ranked) {
			t.Fatalf("pending mutated:\n got %+v\nwant %+v", pending, ranked)
		}
	})
}

func cname(i int) string {
	return fmt.Sprintf("c%02d", i)
}

// TestCadenceQuietWindowMinutes pins the quiet-window decision at minute
// granularity, including the default window that wraps midnight.
func TestCadenceQuietWindowMinutes(t *testing.T) {
	cases := []struct {
		name        string
		prefs       DeliveryPrefs
		minuteOfDay int
		wantQuiet   bool
	}{
		{"default 21:59 is before the window", DefaultDeliveryPrefs(), 21*60 + 59, false},
		{"default 22:00 starts the window", DefaultDeliveryPrefs(), 22 * 60, true},
		{"default 23:59 is inside", DefaultDeliveryPrefs(), 23*60 + 59, true},
		{"default 00:00 wraps into the window", DefaultDeliveryPrefs(), 0, true},
		{"default 03:00 is inside", DefaultDeliveryPrefs(), 3 * 60, true},
		{"default 06:59 is the last quiet minute", DefaultDeliveryPrefs(), 6*60 + 59, true},
		{"default 07:00 ends the window", DefaultDeliveryPrefs(), 7 * 60, false},
		{"default 12:00 is midday", DefaultDeliveryPrefs(), 12 * 60, false},
		{"non-wrap 09:59 is before", DeliveryPrefs{QuietStart: 10 * 60, QuietEnd: 14 * 60}, 9*60 + 59, false},
		{"non-wrap 10:00 starts", DeliveryPrefs{QuietStart: 10 * 60, QuietEnd: 14 * 60}, 10 * 60, true},
		{"non-wrap 13:59 is inside", DeliveryPrefs{QuietStart: 10 * 60, QuietEnd: 14 * 60}, 13*60 + 59, true},
		{"non-wrap 14:00 ends", DeliveryPrefs{QuietStart: 10 * 60, QuietEnd: 14 * 60}, 14 * 60, false},
		{"equal start and end is an empty window", DeliveryPrefs{QuietStart: 12 * 60, QuietEnd: 12 * 60}, 12 * 60, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inQuietHours(wall(tc.minuteOfDay), tc.prefs); got != tc.wantQuiet {
				t.Fatalf("inQuietHours(%02d:%02d) = %v, want %v",
					tc.minuteOfDay/60, tc.minuteOfDay%60, got, tc.wantQuiet)
			}
		})
	}
}

// TestPlanDeliveryDSTLocalClock checks that quiet hours follow the local
// wall clock, not an absolute offset: the same instant expressed in two
// fixed zones (standard vs daylight offset, like the clocks around a DST
// transition) yields different quiet decisions. FixedZone keeps the test
// free of tzdata reads.
func TestPlanDeliveryDSTLocalClock(t *testing.T) {
	est := time.FixedZone("EST", -5*60*60)
	edt := time.FixedZone("EDT", -4*60*60)
	instant := time.Date(2026, 3, 8, 11, 30, 0, 0, time.UTC)
	atEST, atEDT := instant.In(est), instant.In(edt)
	if !atEST.Equal(atEDT) {
		t.Fatalf("test bug: %v and %v are different instants", atEST, atEDT)
	}
	if got, want := atEST.Format("15:04"), "06:30"; got != want {
		t.Fatalf("EST wall clock = %s, want %s", got, want)
	}
	if got, want := atEDT.Format("15:04"), "07:30"; got != want {
		t.Fatalf("EDT wall clock = %s, want %s", got, want)
	}

	item := cand("a", "norm", 10*60)
	prefs := DefaultDeliveryPrefs()

	if _, ok := PlanDelivery(atEST, time.Time{}, []Candidate{item}, nil, prefs, ""); ok {
		t.Fatal("06:30 on the standard-offset clock is quiet: send = true, want false")
	}
	if b, ok := PlanDelivery(atEDT, time.Time{}, []Candidate{item}, nil, prefs, ""); !ok {
		t.Fatal("07:30 on the daylight-offset clock is past quiet hours: send = false, want true")
	} else if got := ids(b.Items); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("items = %v, want [a]", got)
	}
}

func TestFormatBatch(t *testing.T) {
	cases := []struct {
		name string
		b    Batch
		want string
	}{
		{
			name: "empty batch renders empty",
			b:    Batch{},
			want: "",
		},
		{
			name: "one line per item",
			b: Batch{Items: []Candidate{
				{Headline: "First post", Link: "https://example.com/1"},
				{Headline: "Second post", Link: "https://example.com/2"},
			}},
			want: "First post https://example.com/1\nSecond post https://example.com/2",
		},
		{
			name: "overflow collapses into a trailing more line",
			b: Batch{
				Items:    []Candidate{{Headline: "Only shown", Link: "https://example.com/1"}},
				More:     2,
				MoreLink: "https://inbox.example/x",
			},
			want: "Only shown https://example.com/1\n+2 more → https://inbox.example/x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatBatch(tc.b); got != tc.want {
				t.Fatalf("FormatBatch() = %q, want %q", got, tc.want)
			}
		})
	}
}
