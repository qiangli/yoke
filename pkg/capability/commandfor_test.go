package capability

import (
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

func TestCommandForMatchesCapabilityTagAndAliases(t *testing.T) {
	tool := fleet.Tool{Name: "demo", Commands: []fleet.ToolCommand{
		{Name: "exit", Slash: "/exit", Mode: "tui"},                             // no tag: never matches
		{Name: "review", Slash: "/review", Mode: "print", Capability: "review"}, // alias
		{Name: "plan", Slash: "/plan", Mode: "tui", Capability: "planning"},
		{Name: "odd", Slash: "/odd", Mode: "print", Capability: "no-such-capability"},
	}}
	for _, tc := range []struct {
		c    Capability
		want string
	}{
		{CapCodeReview, "review"},
		{CapPlanning, "plan"},
		{CapDeepResearch, ""},
		{CapCoding, ""},
	} {
		got, ok := CommandFor(tool, tc.c)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: matched %q, want none", tc.c, got.Name)
			}
			continue
		}
		if !ok || got.Name != tc.want {
			t.Errorf("%s: got %q (%v), want %q", tc.c, got.Name, ok, tc.want)
		}
	}
}

// The Sprint #626 service verbs resolve to a capability: their own service
// tag, or the quality column they reuse (plan, search). A dotted provider
// role resolves to its head; mail stays the Unix verb and is not an alias.
func TestParseCapabilityServiceTags(t *testing.T) {
	for in, want := range map[string]Capability{
		"search": CapWebSearch, "plan": CapPlanning,
		"image": CapImage, "draw": CapDraw, "pdf": CapPDF, "video": CapVideo,
		"film": CapFilm, "transcribe": CapTranscribe, "narrate": CapNarrate,
		"song": CapSong, "animate": CapAnimate, "email": CapEmail, "drive": CapDrive,
		"image.generate": CapImage, "image.edit": CapImage, "pdf.read": CapPDF,
		"video.understand": CapVideo, "email.send": CapEmail, "drive.write": CapDrive,
		"transcribe.diarize": CapTranscribe, "TTS": CapNarrate, "music": CapSong,
		"diagram": CapDraw, "movie": CapFilm, "stt": CapTranscribe,
	} {
		if got, ok := ParseCapability(in); !ok || got != want {
			t.Errorf("ParseCapability(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"mail", "", ".", "nope.image"} {
		if got, ok := ParseCapability(in); ok {
			t.Errorf("ParseCapability(%q) = %q, want no match", in, got)
		}
	}
	for _, c := range ServiceCaps {
		if slices.Contains(AllCaps(), c) {
			t.Errorf("service tag %s must not be a seeded matrix column", c)
		}
		if !fleet.IsCanonicalToolCommand(string(c)) {
			t.Errorf("service tag %s is not a canonical tool command", c)
		}
	}
	tool := fleet.Tool{Name: "demo", Commands: []fleet.ToolCommand{
		{Name: "image", Slash: "/image", Mode: "print", Capability: "image.generate"},
		{Name: "search", Slash: "/search", Mode: "print", Capability: "search"},
	}}
	if got, ok := CommandFor(tool, CapImage); !ok || got.Name != "image" {
		t.Errorf("CommandFor image = %q, %v", got.Name, ok)
	}
	if got, ok := CommandFor(tool, CapWebSearch); !ok || got.Name != "search" {
		t.Errorf("CommandFor web-search = %q, %v", got.Name, ok)
	}
}
