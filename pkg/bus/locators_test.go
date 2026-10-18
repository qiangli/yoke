package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func pinLocalHost(t *testing.T, h string) {
	t.Helper()
	old := localHostname
	localHostname = func() string { return h }
	t.Cleanup(func() { localHostname = old })
}

func TestMessageLocatorPositives(t *testing.T) {
	pinLocalHost(t, "here")
	body := "evidence: zip:ci.tgz!/logs/test.txt, see `git:origin/main:pkg/lens/row.go` and " +
		"(sprint:446) [[mb:12]] urn:dhnt:todo:2ee2ddcdc0f9. Also zip:ci.tgz!/logs/test.txt again; md:README.md@ws:s446"
	got := MessageLocators(body, "carol")
	want := []string{
		"zip:ci.tgz!/logs/test.txt",
		"git:origin/main:pkg/lens/row.go",
		"sprint:446",
		"mb:12",
		"urn:dhnt:todo:2ee2ddcdc0f9",
		"md:README.md@ws:s446",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d refs %+v, want %v", len(got), got, want)
	}
	for i, w := range want {
		if got[i].Ref != w || got[i].Text != w {
			t.Errorf("ref %d = %+v, want %q", i, got[i], w)
		}
		if got[i].CrossHost || got[i].Host != "" {
			t.Errorf("local sender must not mark a host: %+v", got[i])
		}
	}
	if got[0].Scheme != "zip" || got[1].Scheme != "git" || got[2].Scheme != "sprint" {
		t.Errorf("schemes wrong: %+v", got)
	}
}

func TestMessageLocatorFalsePositives(t *testing.T) {
	pinLocalHost(t, "here")
	for _, body := range []string{
		"note: the build is green at 10:30",
		"see https://example.com/a:b and git://host/repo",
		"plain words /etc/hosts ./rel/path README.md",
		"C:\\Users\\me\\file.txt c:/temp/x",
		"user@host:path scp-style and alice@example.com",
		"Story: #2135 Sprint: #446 Story-ID: 2ee2ddcdc0f9",
		"dir: empty target, sprint: also empty, zip:a!b raw bang, git:main missing path",
		"unknown:thing and X:y",
	} {
		if got := MessageLocators(body, "carol"); len(got) != 0 {
			t.Errorf("body %q produced refs %+v", body, got)
		}
	}
}

func TestMessageLocatorCrossHostPinsOrigin(t *testing.T) {
	pinLocalHost(t, "here.local")
	got := MessageLocators("look at dir:/tmp/out.log and sprint:446 and md:notes.md@far", "bob@there")
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Ref != "dir:/tmp/out.log@there" || got[0].Text != "dir:/tmp/out.log" || !got[0].CrossHost || got[0].Host != "there" || got[0].Note == "" {
		t.Errorf("host-local ref from another host must be pinned to it: %+v", got[0])
	}
	if got[1].Ref != "sprint:446" || got[1].CrossHost || got[1].Host != "" {
		t.Errorf("entity ref is host-independent: %+v", got[1])
	}
	if got[2].Ref != "md:notes.md@far" || !got[2].CrossHost || got[2].Host != "far" {
		t.Errorf("explicit @host scope must be kept and flagged: %+v", got[2])
	}
	// A sender on THIS host is not cross-host; the ref is the canonical bare path.
	same := MessageLocators("dir:/tmp/out.log", "bob@HERE")
	if len(same) != 1 || same[0].CrossHost || same[0].Ref != "/tmp/out.log" || same[0].Host != "HERE" {
		t.Errorf("same-host sender: %+v", same)
	}
}

func TestMessageLocatorBoardRead(t *testing.T) {
	boardInTempHome(t)
	pinLocalHost(t, "here")
	body := "evidence: zip:ci.tgz!/logs/test.txt and dir:/var/log/x.log"
	if err := PostMessage(Post{From: "carol@there", To: "dave", Body: body}); err != nil {
		t.Fatal(err)
	}
	if err := PostMessage(Post{From: "carol", To: "dave", Body: "no references here"}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runMessageBoard(t, context.Background(), "--as", "dave", "--json", "--peek")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two post lines, got %q", out)
	}
	var withR struct {
		Body string       `json:"body"`
		Refs []MessageRef `json:"refs"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &withR); err != nil {
		t.Fatal(err)
	}
	if withR.Body != body {
		t.Fatalf("body must be unchanged, got %q", withR.Body)
	}
	if len(withR.Refs) != 2 || withR.Refs[0].Ref != "zip:ci.tgz!/logs/test.txt@there" || withR.Refs[1].Ref != "dir:/var/log/x.log@there" || !withR.Refs[1].CrossHost {
		t.Fatalf("refs = %+v", withR.Refs)
	}
	if strings.Contains(lines[1], `"refs"`) {
		t.Fatalf("a post with no locators must not gain a refs key: %q", lines[1])
	}

	human, _, err := runMessageBoard(t, context.Background(), "--as", "dave", "--peek")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(human, "  "+body+"\n") {
		t.Fatalf("human view must show the body verbatim:\n%s", human)
	}
	if !strings.Contains(human, "refs: zip:ci.tgz!/logs/test.txt@there (written on there; resolves there, not on this host), dir:/var/log/x.log@there") {
		t.Fatalf("human view must list refs:\n%s", human)
	}
	if strings.Count(human, "refs:") != 1 {
		t.Fatalf("only the post with locators gets a refs line:\n%s", human)
	}
}

func TestLocatorReadCommand(t *testing.T) {
	cases := []struct {
		name string
		refs []MessageRef
		want string
	}{
		{"none", nil, ""},
		{"local", []MessageRef{{Ref: "mb:12"}}, "refs: mb:12 — read: `lens mb:12`"},
		{"remote", []MessageRef{{Ref: "dir:/var/log/x.log@there", Note: "n"}}, "refs: dir:/var/log/x.log@there (n) — read: `lens dir:/var/log/x.log@there`"},
		{"quoted", []MessageRef{{Ref: "zip:a!/b;c"}}, "refs: zip:a!/b;c — read: `lens 'zip:a!/b;c'`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := refsLine(c.refs); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}
