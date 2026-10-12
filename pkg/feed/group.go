package feed

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

type FeedGroup struct {
	ID        string            `json:"id"`
	OwnerUID  string            `json:"owner_uid"`
	Name      string            `json:"name"`
	Title     string            `json:"title"`
	Read      string            `json:"read"`
	Post      string            `json:"post"`
	Join      string            `json:"join"`
	Discovery string            `json:"discovery"`
	Epoch     int               `json:"epoch"`
	Members   []FeedGroupMember `json:"members,omitempty"`
}
type FeedGroupMember struct {
	UserID string `json:"user_id"`
	UID    string `json:"uid,omitempty"`
	Role   string `json:"role"`
}

func (m FeedGroupMember) userID() string {
	if m.UserID != "" {
		return m.UserID
	}
	return m.UID
}

type FeedGroupKey struct {
	Epoch          int               `json:"epoch"`
	Wrapped        string            `json:"wrapped"`
	WrappedDevices map[string]string `json:"wrapped_devices"`
}

type FeedEncDevice struct {
	Pubkey string `json:"pubkey"`
}

type GroupClient struct {
	Base, Token string
	HTTP        *http.Client
}

func NewGroupClient(env Env) (*GroupClient, error) {
	cloud := env.Cloud
	if cloud == nil {
		cloud = func() (string, string) { return fleet.ResolveCloud("", "") }
	}
	base, token := cloud()
	if token == "" {
		return nil, errors.New("feed: host is not paired with cloudbox")
	}
	if base == "" {
		base = "https://ai.dhnt.io"
	}
	return &GroupClient{Base: strings.TrimRight(base, "/"), Token: token, HTTP: env.HTTP}, nil
}
func (c *GroupClient) request(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api/v1/feed/"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: syncTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := serverReason(raw)
		if reason == "" {
			reason = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("feed: group API HTTP %d: %s", resp.StatusCode, reason)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("feed: group API response: %w", err)
		}
	}
	return nil
}
func (c *GroupClient) PutEncKey(ctx context.Context, pubkey string) error {
	return c.request(ctx, http.MethodPut, "enckey", map[string]string{"pubkey": pubkey}, nil)
}
func (c *GroupClient) GetEncKey(ctx context.Context, uid string) (string, error) {
	var result struct {
		Pubkey string `json:"pubkey"`
	}
	err := c.request(ctx, http.MethodGet, "enckey/"+url.PathEscape(uid), nil, &result)
	if err == nil && result.Pubkey == "" {
		err = errors.New("feed: member has no encryption pubkey")
	}
	return result.Pubkey, err
}
func (c *GroupClient) EncDevices(ctx context.Context, uid string) ([]FeedEncDevice, error) {
	var result struct {
		Devices []FeedEncDevice `json:"devices"`
	}
	err := c.request(ctx, http.MethodGet, "enc-devices/"+url.PathEscape(uid), nil, &result)
	if err != nil {
		return nil, err
	}
	if len(result.Devices) == 0 {
		return nil, errors.New("feed: member has no encryption devices")
	}
	return result.Devices, nil
}
func (c *GroupClient) PutDeviceWrap(ctx context.Context, id string, epoch int, uid, pubkey, wrapped string) error {
	return c.request(ctx, http.MethodPut, "groups/"+url.PathEscape(id)+"/device-wrap", map[string]any{
		"epoch": epoch, "uid": uid, "pubkey": pubkey, "wrapped": wrapped,
	}, nil)
}

// WrapDevices includes every registered encryption device, with the legacy
// primary wrap returned separately for older readers.
func (c *GroupClient) WrapDevices(ctx context.Context, uid string, key []byte, group string, epoch int) (string, map[string]string, error) {
	primary, err := c.GetEncKey(ctx, uid)
	if err != nil {
		return "", nil, err
	}
	devices, err := c.EncDevices(ctx, uid)
	if err != nil {
		return "", nil, err
	}
	wrapped, err := Wrap(key, primary, group, epoch, nil)
	if err != nil {
		return "", nil, err
	}
	additional := make(map[string]string)
	seen := map[string]bool{primary: true}
	for _, d := range devices {
		if seen[d.Pubkey] {
			continue
		}
		seen[d.Pubkey] = true
		additional[d.Pubkey], err = Wrap(key, d.Pubkey, group, epoch, nil)
		if err != nil {
			return "", nil, err
		}
	}
	return wrapped, additional, nil
}
func (c *GroupClient) CreateGroup(ctx context.Context, name, title, preset, wrapped string) (FeedGroup, error) {
	body := map[string]any{"name": name, "title": title, "preset": preset}
	if wrapped != "" {
		body["wrapped"] = wrapped
	}
	return c.createGroup(ctx, body)
}
func (c *GroupClient) ListGroups(ctx context.Context) ([]FeedGroup, error) {
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodGet, "groups", nil, &raw); err != nil {
		return nil, err
	}
	var groups []FeedGroup
	if err := json.Unmarshal(raw, &groups); err == nil {
		return groups, nil
	}
	var envelope struct {
		Groups []FeedGroup `json:"groups"`
	}
	err := json.Unmarshal(raw, &envelope)
	return envelope.Groups, err
}
func (c *GroupClient) GetGroup(ctx context.Context, id string) (FeedGroup, error) {
	var raw json.RawMessage
	err := c.request(ctx, http.MethodGet, "groups/"+url.PathEscape(id), nil, &raw)
	if err != nil {
		return FeedGroup{}, err
	}
	var g FeedGroup
	if err = json.Unmarshal(raw, &g); err != nil {
		return g, err
	}
	if g.ID == "" {
		var envelope struct {
			Group   FeedGroup         `json:"group"`
			Members []FeedGroupMember `json:"members"`
		}
		if err = json.Unmarshal(raw, &envelope); err != nil {
			return g, err
		}
		g = envelope.Group
		g.Members = envelope.Members
	}
	return g, nil
}
func (c *GroupClient) AddMember(ctx context.Context, id, uid, wrapped string) error {
	return c.AddMemberWithDevices(ctx, id, uid, wrapped, nil)
}
func (c *GroupClient) AddMemberWithDevices(ctx context.Context, id, uid, wrapped string, devices map[string]string) error {
	request := map[string]any{"uid": uid, "wrapped": wrapped}
	if len(devices) > 0 {
		request["wrapped_devices"] = devices
	}
	return c.request(ctx, http.MethodPost, "groups/"+url.PathEscape(id)+"/members", request, nil)
}
func (c *GroupClient) RemoveMember(ctx context.Context, id, uid string, epoch int, keys map[string]string) error {
	return c.RemoveMemberWithDevices(ctx, id, uid, epoch, keys, nil)
}
func (c *GroupClient) RemoveMemberWithDevices(ctx context.Context, id, uid string, epoch int, keys map[string]string, devices map[string]map[string]string) error {
	body := map[string]any{}
	if epoch > 0 {
		body["epoch"] = epoch
		body["keys"] = keys
		body["device_keys"] = devices
	}
	return c.request(ctx, http.MethodDelete, "groups/"+url.PathEscape(id)+"/members/"+url.PathEscape(uid), body, nil)
}
func (c *GroupClient) GroupKeys(ctx context.Context, id string) ([]FeedGroupKey, error) {
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodGet, "groups/"+url.PathEscape(id)+"/keys", nil, &raw); err != nil {
		return nil, err
	}
	var keys []FeedGroupKey
	if err := json.Unmarshal(raw, &keys); err == nil {
		return keys, nil
	}
	var envelope struct {
		Keys []FeedGroupKey `json:"keys"`
	}
	err := json.Unmarshal(raw, &envelope)
	return envelope.Keys, err
}

func groupCachePath(dir, id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return "", errors.New("feed: invalid group id")
	}
	return filepath.Join(dir, "groups", id+".json"), nil
}
func LoadGroupKey(dir, id string, epoch int) ([]byte, error) {
	path, err := groupCachePath(dir, id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := checkKeyPerms(path); err != nil {
		return nil, err
	}
	var data map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(data[strconv.Itoa(epoch)])
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("feed: group key unavailable")
	}
	return key, nil
}
func SaveGroupKey(dir, id string, epoch int, key []byte) error {
	if epoch < 1 || len(key) != 32 {
		return errors.New("feed: invalid group key")
	}
	path, err := groupCachePath(dir, id)
	if err != nil {
		return err
	}
	data := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := checkKeyPerms(path); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	data[strconv.Itoa(epoch)] = base64.StdEncoding.EncodeToString(key)
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".group-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func CurrentGroupKey(ctx context.Context, c *GroupClient, dir, id string, epoch int) ([]byte, error) {
	if key, err := LoadGroupKey(dir, id, epoch); err == nil {
		return key, nil
	}
	priv, err := LoadOrCreateEncKey(dir)
	if err != nil {
		return nil, err
	}
	keys, err := c.GroupKeys(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, entry := range keys {
		if entry.Epoch != epoch {
			continue
		}
		wrapped := entry.Wrapped
		if own, ok := entry.WrappedDevices[EncPubkeyB64(priv)]; ok {
			wrapped = own
		}
		key, err := Unwrap(wrapped, priv, id, epoch)
		if err != nil {
			return nil, err
		}
		if err := SaveGroupKey(dir, id, epoch, key); err != nil {
			return nil, err
		}
		return key, nil
	}
	return nil, fmt.Errorf("feed: no key for group %s epoch %d", id, epoch)
}
func GenerateGroupKey(random io.Reader) ([]byte, error) {
	key := make([]byte, 32)
	_, err := io.ReadFull(randomReader(random), key)
	return key, err
}

func freshGroupKey() ([]byte, error) { return GenerateGroupKey(rand.Reader) }

func (c *GroupClient) CreateGroupWithID(ctx context.Context, id, name, title, preset, wrapped string) (FeedGroup, error) {
	return c.CreateGroupWithDevices(ctx, id, name, title, preset, wrapped, nil)
}
func (c *GroupClient) CreateGroupWithDevices(ctx context.Context, id, name, title, preset, wrapped string, devices map[string]string) (FeedGroup, error) {
	body := map[string]any{"id": id, "name": name, "title": title, "preset": preset, "wrapped": wrapped}
	if len(devices) > 0 {
		body["wrapped_devices"] = devices
	}
	return c.createGroup(ctx, body)
}

func (c *GroupClient) createGroup(ctx context.Context, body map[string]any) (FeedGroup, error) {
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodPost, "groups", body, &raw); err != nil {
		return FeedGroup{}, err
	}
	var g FeedGroup
	if err := json.Unmarshal(raw, &g); err != nil {
		return g, err
	}
	if g.ID == "" {
		var envelope struct {
			Group FeedGroup `json:"group"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return g, err
		}
		g = envelope.Group
	}
	return g, nil
}

func indexGroupedPublishedPost(ctx context.Context, env Env, dir string, warn io.Writer, base, token string, p *Post, req IndexRequest) {
	raw, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(warn, "index: failed: %v\n", err)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		fmt.Fprintf(warn, "index: failed: %v\n", err)
		return
	}
	post := body["post"].(map[string]any)
	post["group"] = p.Group
	if p.Epoch > 0 {
		post["epoch"] = p.Epoch
	}
	c := &GroupClient{Base: strings.TrimRight(base, "/"), Token: token, HTTP: env.HTTP}
	var resp IndexResponse
	err = c.request(ctx, http.MethodPost, "registry", body, &resp)
	if err == nil && resp.Indexed != "deferred" {
		noteRegistryKey(dir, base, p.Pubkey)
	}
	reportIndexResult(warn, resp, err)
}

type fetchedGroupMeta struct {
	Group string `json:"group"`
	Epoch int    `json:"epoch"`
	Enc   string `json:"enc"`
}

func fetchedGroupPath(dir, id, hash string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") || len(hash) != 64 {
		return "", errors.New("feed: invalid post id")
	}
	for _, c := range hash {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return "", errors.New("feed: invalid post hash")
		}
	}
	return filepath.Join(dir, "groups", "posts", id+"-"+hash+".json"), nil
}
func cacheFetchedGroupPost(dir, id, hash string, raw []byte) {
	p, err := ParsePost(raw)
	if err != nil || p.Group == "" {
		return
	}
	path, err := fetchedGroupPath(dir, id, hash)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(fetchedGroupMeta{p.Group, p.Epoch, p.Enc})
	if err != nil {
		return
	}
	// This file contains metadata only. Atomic replacement avoids torn reads.
	f, err := os.CreateTemp(filepath.Dir(path), ".post-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	_ = os.Rename(f.Name(), path)
}
func decryptGroupEntries(ctx context.Context, env Env, dir string, entries []SpoolEntry) []SpoolEntry {
	out := make([]SpoolEntry, len(entries))
	copy(out, entries)
	var client *GroupClient
	for i := range out {
		path, err := fetchedGroupPath(dir, out[i].ID, out[i].Hash)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta fetchedGroupMeta
		if json.Unmarshal(raw, &meta) != nil || meta.Enc != "v1" {
			continue
		}
		placeholder := fmt.Sprintf("[encrypted: group %s epoch %d]", meta.Group, meta.Epoch)
		if client == nil {
			client, _ = NewGroupClient(env)
		}
		var key []byte
		key, err = LoadGroupKey(dir, meta.Group, meta.Epoch)
		if err != nil && client != nil {
			key, err = CurrentGroupKey(ctx, client, dir, meta.Group, meta.Epoch)
		}
		if err != nil {
			out[i].Body = placeholder
			out[i].Headline = placeholder
			out[i].Dek = placeholder
			continue
		}
		p := &Post{Group: meta.Group, Epoch: meta.Epoch, Enc: meta.Enc, Body: out[i].Body, Headline: out[i].Headline, Dek: out[i].Dek}
		plain, err := DecryptPost(p, key)
		if err != nil {
			out[i].Body = placeholder
			out[i].Headline = placeholder
			out[i].Dek = placeholder
			continue
		}
		out[i].Body = plain.Body
		out[i].Headline = plain.Headline
		out[i].Dek = plain.Dek
	}
	return out
}
