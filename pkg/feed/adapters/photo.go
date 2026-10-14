package adapters

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/feed"
)

// downloadPhoto keeps the vendor URL and any token out of error strings.
// The envelope owns the resulting local file for downstream bridge delivery.
func downloadPhoto(ctx context.Context, client *http.Client, source, token, name, mime string) (feed.Attachment, error) {
	u, err := url.Parse(source)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
		return feed.Attachment{}, errors.New("feed photo: invalid URL")
	}
	if token != "" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "slack.com" && !strings.HasSuffix(u.Hostname(), ".slack.com") {
		return feed.Attachment{}, errors.New("feed photo: invalid Slack host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return feed.Attachment{}, errors.New("feed photo: invalid request")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return feed.Attachment{}, errors.New("feed photo: download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return feed.Attachment{}, errors.New("feed photo: download failed")
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") && !strings.HasPrefix(mime, "image/") {
		return feed.Attachment{}, errors.New("feed photo: response is not an image")
	}
	f, err := os.CreateTemp("", "feed-photo-*")
	if err != nil {
		return feed.Attachment{}, err
	}
	defer f.Close()
	path := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, (8<<20)+1))
	if copyErr != nil || n > 8<<20 {
		err = errors.New("feed photo: download exceeds limit or failed")
		return feed.Attachment{}, err
	}
	if err = f.Close(); err != nil {
		return feed.Attachment{}, err
	}
	att, err := feed.NewAttachment(path, mime)
	if err != nil {
		return feed.Attachment{}, err
	}
	att.Name = filepath.Base(name)
	return att, nil
}
