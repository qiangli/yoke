package ref

import "testing"

func TestLocatorRoundTrip(t *testing.T) {
	for _, input := range []string{
		"zip:a%21/b.zip", "zip:a.zip!/b.tgz!/c.pdf", "git:origin/main:pkg/file.go",
		"net:https%3A//example/a%40b", "run:abc@host1", "run:abc@ws:clone1",
		"dir:C%3A%5Cwork%5Cfile", "oci:images@k8s:dev", "k8s:pods@cloud:prod",
		"urn:dhnt:run:abc", "run:abc%40def@ws:clone1",
	} {
		loc, err := ParseLocator(input)
		if err != nil {
			t.Errorf("ParseLocator(%q): %v", input, err)
			continue
		}
		if got := loc.String(); got != input {
			t.Errorf("%q canonicalized to %q", input, got)
		}
	}
}

func TestLocatorRejectsAmbiguity(t *testing.T) {
	for _, input := range []string{"zip:a!b", "zip:a.zip!/", "zip:a@ws:x@host", "zip:a%ZZ", "net:https://example", "git:rev:path:more", "s3:x", "run:x@unknown:y"} {
		if _, err := ParseLocator(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestLocatorWindowsDrive(t *testing.T) {
	loc, err := ParseLocator(`C:\work\file`)
	if err != nil || loc.Scheme != "dir" || loc.Target != `C:\work\file` {
		t.Fatalf("loc=%+v err=%v", loc, err)
	}
	if got := loc.String(); got != "dir:C%3A%5Cwork%5Cfile" {
		t.Fatalf("canonical Windows path = %q", got)
	}
}
