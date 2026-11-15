package principal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadOutpostAgentNamePrefersCloudboxIdentity(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct{ body, want string }{
		"identity only (unattached host)": {`{"cloudbox":{"portal_url":"https://m.example","host":"win1"}}`, "win1"},
		"identity wins over legacy":       {`{"agent_name":"legacy","cloudbox":{"host":"win2"}}`, "win2"},
		"legacy top-level":                {`{"agent_name":"old-box"}`, "old-box"},
		"attachment tunnel only":          {`{"attachments":[{"satellite_id":"s","tunnel":{"agent_name":"tun"}}]}`, "tun"},
		"paired, name unknown":            {`{"server_addr":"x"}`, ""},
	} {
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := readOutpostAgentName(p)
		if !ok || got != tc.want {
			t.Fatalf("%s: got %q ok=%v, want %q", name, got, ok, tc.want)
		}
	}
	if _, ok := readOutpostAgentName(filepath.Join(dir, "missing.json")); ok {
		t.Fatal("missing file reported paired")
	}
}
