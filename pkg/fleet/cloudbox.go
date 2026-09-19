package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/assetring"
)

// The optional org overlay.
//
// A paired host can pull a shared catalog from an org control plane: an
// additive ring between the shared dirs and the local store. It is an
// enhancement, never a gate. Every verb in this package answers from the
// compiled-in baseline with no network, no account, and no config — the
// overlay only adds entries, and a local entry still shadows it.
//
// The wire is a Bearer HTTP API serving the same YAML documents this package
// writes to disk, so a `sync` is a download, not a translation. Once cached,
// the ring reads from disk like any other: an unreachable control plane
// degrades to the last good pull rather than to an error.

// CloudConfig resolves how to reach the overlay.
type CloudConfig struct {
	URL   string
	Token string
}

// Resolve fills the base URL and Bearer token, in order:
//
//	URL:   --url flag > $BASHY_CLOUDBOX_URL > https://ai.dhnt.io
//	Token: --token flag > $BASHY_FLEET_TOKEN > $BASHY_API_KEY > paired outpost
//
// The token needs the read scopes for the nouns being synced. Minting it
// read-only is the point: a token that pulls a catalog should not be able to
// rewrite it.
func (c CloudConfig) Resolve() (CloudClient, error) {
	base, tok := ResolveCloud(c.URL, c.Token)
	if tok == "" {
		return CloudClient{}, fmt.Errorf("fleet: no cloudbox token (set $BASHY_FLEET_TOKEN or pass --token); the registry works fine without one")
	}
	return CloudClient{BaseURL: base, Token: tok, HTTP: &http.Client{Timeout: 20 * time.Second}}, nil
}

// ResolveCloud is the ONE ladder every bashy-side cloudbox client walks, so a
// paired host needs no per-feature token setup:
//
//	URL:   override > $BASHY_CLOUDBOX_URL > https://ai.dhnt.io
//	Token: override > $BASHY_FLEET_TOKEN > $BASHY_API_KEY > paired outpost
//
// The paired outpost's token is the derived credential — pairing is the only
// setup a developer does. An empty token means "not paired and nothing set";
// the caller words the refusal for its own feature.
func ResolveCloud(urlOverride, tokenOverride string) (base, token string) {
	base = strings.TrimRight(firstNonEmptyStr(urlOverride, os.Getenv("BASHY_CLOUDBOX_URL"), "https://ai.dhnt.io"), "/")
	token = firstNonEmptyStr(tokenOverride, os.Getenv("BASHY_FLEET_TOKEN"), os.Getenv("BASHY_API_KEY"))
	if token == "" {
		token = outpostToken()
	}
	return base, token
}

func outpostToken() string {
	path, err := exec.LookPath("outpost")
	if err != nil {
		return ""
	}
	out, err := exec.Command(path, "token", "print").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// CloudClient reads an org catalog over the Bearer asset API.
type CloudClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// cloudAsset is the shared read shape for the Content-blob nouns.
type cloudAsset struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Mode    string `json:"mode"`
}

// cloudModel is the models read shape, whose definition lives in structured
// fields rather than a Content blob.
type cloudModel struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	Content string `json:"content"`
	Status  string `json:"status"`
	Mode    string `json:"mode"`

	Source string `json:"source"`
	Kind   string `json:"kind"`

	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	APIKeyRef string `json:"api_key_ref"`
	Model     string `json:"model"` // provider-side id

	// Family, Version, and Band ride the overlay so an org can re-peg a
	// band, or publish a new version of a family, without anyone shipping
	// a binary. A server that omits them leaves the embedded peg standing.
	Family  string `json:"family"`
	Version string `json:"version"`
	Band    int    `json:"band"`

	Tier          string   `json:"tier"`
	Capabilities  []string `json:"capabilities"`
	Domain        []string `json:"domain"`
	ContextLength int64    `json:"context_length"`
	Price         float64  `json:"price"`
}

func (c CloudClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("fleet: GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SyncResult reports what a pull wrote.
type SyncResult struct {
	Noun    string `json:"noun"`
	Fetched int    `json:"fetched"`
	Skipped int    `json:"skipped,omitempty"`
	Dir     string `json:"dir"`
}

// Sync pulls one noun's org catalog into the overlay cache.
//
// Tools are filtered to the agentic-CLI kind. The asset registry's tool
// namespace is shared with MCP-style function kits, and a kit is not something
// the fleet can launch — pulling one into the tool ring would list a name that
// `verify` can only ever report as unusable.
func (c CloudClient) Sync(cacheRoot, noun string) (SyncResult, error) {
	res := SyncResult{Noun: noun, Dir: filepath.Join(cacheRoot, noun)}

	var docs map[string][]byte
	var err error
	switch noun {
	case dirModels:
		docs, err = c.fetchModels()
	case dirTools:
		docs, res.Skipped, err = c.fetchTools()
	case dirAgents, dirSkills:
		docs, err = c.fetchAssets(noun)
	default:
		return res, fmt.Errorf("fleet: cannot sync %q", noun)
	}
	if err != nil {
		return res, err
	}

	// Replace the noun's cache wholesale: an entry deleted upstream must
	// disappear here, and a partial merge would resurrect it forever.
	dir := res.Dir
	if err := os.RemoveAll(dir); err != nil {
		return res, err
	}
	for name, body := range docs {
		write := writeEntry
		if noun == dirSkills {
			write = writeSkillEntry
		}
		if err := write(dir, name, body); err != nil {
			return res, err
		}
	}
	res.Fetched = len(docs)
	return res, nil
}

func (c CloudClient) fetchAssets(noun string) (map[string][]byte, error) {
	var env map[string][]cloudAsset
	if err := c.get("/api/v1/"+noun, &env); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, a := range env[noun] {
		if validName(a.Name) != nil {
			continue
		}
		out[a.Name] = []byte(a.Content)
	}
	return out, nil
}

func (c CloudClient) fetchTools() (map[string][]byte, int, error) {
	var env map[string][]cloudAsset
	if err := c.get("/api/v1/tools", &env); err != nil {
		return nil, 0, err
	}
	out, skipped := map[string][]byte{}, 0
	for _, a := range env["tools"] {
		if validName(a.Name) != nil {
			continue
		}
		t, err := ParseTool(a.Name, []byte(a.Content), nil)
		if err != nil || !t.IsCLI() {
			skipped++ // a function kit, or a document we cannot read
			continue
		}
		out[a.Name] = []byte(a.Content)
	}
	return out, skipped, nil
}

// fetchModels renders the structured columns back into the canonical YAML this
// package stores on disk, so an overlay entry is indistinguishable from a
// local one once cached.
func (c CloudClient) fetchModels() (map[string][]byte, error) {
	var env map[string][]cloudModel
	if err := c.get("/api/v1/models", &env); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, m := range env["models"] {
		if validName(m.Name) != nil {
			continue
		}
		if m.Band < 0 || m.Band > MaxBand {
			continue // a nonsense band would silently misroute; drop the row
		}
		body, err := Marshal(Model{
			Name: m.Name, Display: m.Display, Kind: m.Kind, Source: m.Source,
			Provider: m.Provider, BaseURL: m.BaseURL, APIKeyRef: m.APIKeyRef,
			UpstreamID: m.Model, Tier: m.Tier,
			Family: m.Family, Version: m.Version, Band: m.Band,
			Capabilities: m.Capabilities, Domain: m.Domain,
			ContextLength: m.ContextLength, Price: m.Price,
		})
		if err != nil {
			return nil, err
		}
		out[m.Name] = body
	}
	return out, nil
}

// dirSkills is accepted by Sync even though this package does not read skills;
// pkg/skills owns that ring. Pulling them here keeps one `sync` verb.
const dirSkills = "skills"

// writeSkillEntry writes one synced skill as <dir>/<name>/SKILL.md.
//
// Skills are FOLDER-shaped — pkg/skills reads them with a folder ring keyed on
// a SKILL.md marker — while tools, models and agents are single YAML documents
// that writeEntry spells <name>.yaml. Using the flat writer for skills pulled
// them into a layout the reader ignores: `sync` reported "1 pulled" and
// `skills list` showed nothing, which reads as an empty org catalog rather
// than a layout mismatch. The pull and the read must agree on shape, not just
// on path.
func writeSkillEntry(dir, name string, data []byte) error {
	if err := validName(name); err != nil {
		return err
	}
	folder := filepath.Join(dir, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(folder, "SKILL.md"), data, 0o644)
}

// CloudCacheRoot is where a pulled overlay lives.
func CloudCacheRoot(root string) string { return filepath.Join(root, "fleet", "cloud-cache") }

// cloudSources returns the overlay ring for a noun, or nothing when no pull
// has ever landed. A missing cache is an empty ring, not an error: an unpaired
// host is the normal case.
func cloudSources(root, noun string) []assetring.Source {
	dir := filepath.Join(CloudCacheRoot(root), noun)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	return []assetring.Source{assetring.FileDir(dir, assetring.RingCloud, ext)}
}
