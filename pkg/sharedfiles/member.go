package sharedfiles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/toolcmd"
	"github.com/spf13/cobra"
)

// Target identifies a path relative to one designated share on a peer.
type Target struct{ Host, Name, Path string }

// ParseTarget accepts HOST:NAME[/PATH] or a bashy://HOST/files-NAME[/PATH] URI.
func ParseTarget(raw string) (Target, error) {
	var t Target
	var sharePath string
	if strings.HasPrefix(raw, "bashy://") {
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || !strings.HasPrefix(u.Path, "/files-") || strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") {
			return t, fmt.Errorf("invalid bashy files URI")
		}
		t.Host, sharePath = u.Host, strings.TrimPrefix(u.Path, "/files-")
	} else {
		var ok bool
		t.Host, sharePath, ok = strings.Cut(raw, ":")
		if !ok {
			return t, fmt.Errorf("expected HOST:NAME[/PATH]")
		}
	}
	if !nameRE.MatchString(t.Host) {
		return t, fmt.Errorf("invalid peer name")
	}
	t.Name, t.Path, _ = strings.Cut(sharePath, "/")
	if err := checkName(t.Name); err != nil {
		return t, err
	}
	if t.Path != "" {
		for _, s := range strings.Split(t.Path, "/") {
			if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "\\\x00\r\n") {
				return t, fmt.Errorf("invalid share-relative path")
			}
		}
	}
	return t, nil
}

type call struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

var client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Fetch streams a designated peer file into dst. The caller chooses dst inside
// its private run directory; credentials are loaded from the local peer store.
func Fetch(ctx context.Context, uri, dst string) error {
	if !strings.HasPrefix(uri, "bashy://") {
		return fmt.Errorf("expected bashy:// files URI")
	}
	t, err := ParseTarget(uri)
	if err != nil || t.Path == "" {
		return fmt.Errorf("invalid bashy files URI")
	}
	door, err := toolcmd.LoadPeer(t.Host)
	if err != nil {
		return err
	}
	endpoint, err := url.JoinPath(door.URL+"/files/"+t.Name, t.Path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+door.Credential)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("files get: peer request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("files get: peer returned HTTP %d", resp.StatusCode)
	}
	_, err = download(dst, resp.Body)
	return err
}

func memberCmd(verb string, asJSON *bool) *cobra.Command {
	var offset, length int64
	cmd := &cobra.Command{Use: verb + " HOST:NAME[/PATH]", Args: cobra.ExactArgs(1)}
	if verb == "get" {
		cmd.Use += " [DEST]"
		cmd.Args = cobra.RangeArgs(1, 2)
	}
	if verb == "cat" {
		cmd.Flags().Int64Var(&offset, "offset", 0, "First byte to read")
		cmd.Flags().Int64Var(&length, "length", 0, "Number of bytes (default: door limit)")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		t, err := ParseTarget(args[0])
		if err != nil {
			return err
		}
		if offset < 0 || length < 0 {
			return fmt.Errorf("offset and length must be nonnegative")
		}
		door, err := toolcmd.LoadPeer(t.Host)
		if err != nil {
			return err
		}
		endpoint := door.URL + "/files/" + t.Name
		method := http.MethodPost
		var payload io.Reader
		if verb == "get" {
			if t.Path == "" {
				return fmt.Errorf("get requires a file path")
			}
			endpoint, err = url.JoinPath(endpoint, (&url.URL{Path: t.Path}).EscapedPath())
			if err != nil {
				return err
			}
			method = http.MethodGet
		} else {
			op := verb
			if op == "cat" {
				op = "read"
			}
			body, err := json.Marshal(call{Op: op, Path: t.Path, Offset: offset, Length: length})
			if err != nil {
				return err
			}
			payload = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(cmd.Context(), method, endpoint, payload)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+door.Credential)
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("files %s: peer request failed: %w", verb, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("files %s: peer returned HTTP %d", verb, resp.StatusCode)
		}
		out := cmd.OutOrStdout()
		if verb == "get" {
			dest := filepath.Base(t.Path)
			if len(args) > 1 {
				dest = args[1]
			}
			n, err := download(dest, resp.Body)
			if err != nil {
				return err
			}
			if *asJSON {
				return json.NewEncoder(out).Encode(map[string]any{"path": dest, "bytes": n})
			}
			return nil
		}
		var result json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(result)
		}
		switch verb {
		case "cat":
			var read struct {
				Data *[]byte `json:"data"`
			}
			if err = json.Unmarshal(result, &read); err != nil {
				return err
			}
			if read.Data == nil {
				return fmt.Errorf("missing inline data; use files get to stream the file")
			}
			_, err = out.Write(*read.Data)
		case "link":
			var link struct {
				URI string `json:"uri"`
			}
			if err = json.Unmarshal(result, &link); err != nil {
				return err
			}
			if link.URI == "" {
				return fmt.Errorf("missing link URI")
			}
			_, err = fmt.Fprintln(out, link.URI)
		default:
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			err = enc.Encode(result)
		}
		return err
	}
	return cmd
}

func download(dest string, src io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, src)
	closeErr := tmp.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return n, os.Rename(tmp.Name(), dest)
}
