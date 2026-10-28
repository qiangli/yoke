package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/guard"
	"github.com/qiangli/yoke/pkg/browser/internal/cdpactions"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

const Mode = "probe"

type Client struct {
	url string

	mu        sync.Mutex
	allocCtx  context.Context
	allocStop context.CancelFunc
	ctx       context.Context
	ctxStop   context.CancelFunc
}

func New(url string) *Client {
	if url == "" {
		url = DefaultURL
	}
	return &Client{url: strings.TrimRight(url, "/")}
}

func (c *Client) URL() string { return c.url }

func (c *Client) Available(ctx context.Context) bool {
	hctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, c.url+"/json/version", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *Client) EnsureReady(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx != nil {
		return nil
	}
	if !c.Available(ctx) {
		return fmt.Errorf("probe: no Chrome at %s", c.url)
	}
	allocCtx, allocStop := chromedp.NewRemoteAllocator(context.Background(), c.url)
	options := []chromedp.ContextOption{}
	if id := c.existingPageTarget(ctx); id != "" {
		options = append(options, chromedp.WithTargetID(target.ID(id)))
	}
	cdpCtx, cdpStop := chromedp.NewContext(allocCtx, options...)
	if err := chromedp.Run(cdpCtx); err != nil {
		cdpStop()
		allocStop()
		return fmt.Errorf("probe: attach to %s: %w", c.url, err)
	}
	c.allocCtx, c.allocStop = allocCtx, allocStop
	c.ctx, c.ctxStop = cdpCtx, cdpStop
	return nil
}

type pageTarget struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

// existingPageTarget keeps probe mode attached to the browser's page instead
// of creating a disposable about:blank target for every one-shot CLI command.
// Prefer real pages, then an existing blank page that navigate can reuse.
func (c *Client) existingPageTarget(ctx context.Context) string {
	hctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, c.url+"/json/list", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var targets []pageTarget
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&targets) != nil {
		return ""
	}
	return selectPageTarget(targets)
}

func selectPageTarget(targets []pageTarget) string {
	blank := ""
	for _, candidate := range targets {
		if candidate.Type != "page" {
			continue
		}
		if candidate.URL != "" && candidate.URL != "about:blank" && !strings.HasPrefix(candidate.URL, "chrome://") {
			return candidate.ID
		}
		if blank == "" {
			blank = candidate.ID
		}
	}
	return blank
}

// Execute runs one action. Every action passes the sensitive-input guard
// first (pkg/browser/guard); the field check reuses the live extension's
// guard.js in a CDP isolated world.
func (c *Client) Execute(ctx context.Context, action wire.Action) (*wire.Result, error) {
	gb := cdpactions.GuardBackend{Ready: c.ready, Current: c.current, Human: true}
	return guard.Execute(ctx, gb, action, func(ctx context.Context, a wire.Action) (*wire.Result, error) {
		cdpCtx, err := c.ready(ctx)
		if err != nil {
			return nil, err
		}
		callCtx, cancel := context.WithTimeout(cdpCtx, 30*time.Second)
		defer cancel()
		return cdpactions.Run(callCtx, Mode, a)
	})
}

// current is the attached chromedp context, or nil before EnsureReady.
func (c *Client) current() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctx
}

// ready attaches on first use and returns the chromedp context.
func (c *Client) ready(ctx context.Context) (context.Context, error) {
	if cdpCtx := c.current(); cdpCtx != nil {
		return cdpCtx, nil
	}
	if err := c.EnsureReady(ctx); err != nil {
		return nil, err
	}
	if cdpCtx := c.current(); cdpCtx != nil {
		return cdpCtx, nil
	}
	return nil, errors.New("probe: not ready")
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A remote probe attaches to an operator-owned tab. chromedp normally
	// closes every non-root target when its context is cancelled, which made a
	// sequence of one-shot `bashy browser` commands destroy the page after each
	// action. Clear only chromedp's cleanup handle before cancelling; closing the
	// remote allocator still detaches the websocket while Chrome retains the tab.
	if c.ctx != nil {
		if state := chromedp.FromContext(c.ctx); state != nil {
			state.Target = nil
		}
	}
	if c.ctxStop != nil {
		c.ctxStop()
		c.ctxStop = nil
	}
	if c.allocStop != nil {
		c.allocStop()
		c.allocStop = nil
	}
	c.ctx = nil
	c.allocCtx = nil
	return nil
}
