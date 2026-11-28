package fleet

import "testing"

func TestSatelliteAppBase(t *testing.T) {
	const base = "https://sat.example/matrix/h/peer/app/offer-files-demo"
	for _, suffix := range []string{"", "/", "/files/demo/file%20name"} {
		e, err := ParseSatelliteAppBase(base + suffix)
		if err != nil || e.App != "offer-files-demo" || e.Host != "peer" || e.Satellite != "https://sat.example" {
			t.Fatal(e, err)
		}
	}
	for _, raw := range []string{base + "?key=secret", base + "#frag", base + "/%2e%2e", base + "/a%2fb", base + "/%5c", "http://sat.example/matrix/h/peer/app/offer-x", "https://sat.example/matrix/h/peer/app/files-x", "https://sat.example/matrix/h/peer/app/offer-", "https://user@sat.example/matrix/h/peer/app/offer-demo"} {
		if _, err := ParseSatelliteAppBase(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := ParseSatelliteAppURL(base); err == nil {
		t.Error("MCP parser accepted bare offer route")
	}
}
