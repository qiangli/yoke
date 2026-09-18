package solo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/internal/cdpactions"
	"github.com/qiangli/yoke/pkg/browser/probe"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

const Mode = "solo"

type Config struct {
	ChromePath  string
	Headed      bool
	UserDataDir string
}

type Client struct {
	cfg Config

	mu        sync.Mutex
	allocCtx  context.Context
	allocStop context.CancelFunc
	ctx       context.Context
	ctxStop   context.CancelFunc

	tempUserData string
}

func New(cfg Config) *Client { return &Client{cfg: cfg} }

func (c *Client) Config() Config { return c.cfg }

func (c *Client) Available(ctx context.Context) bool {
	return c.ResolveChromePath() != ""
}

func (c *Client) ResolveChromePath() string {
	if c.cfg.ChromePath != "" {
		if _, err := os.Stat(c.cfg.ChromePath); err == nil {
			return c.cfg.ChromePath
		}
		return ""
	}
	return probe.DetectChrome()
}

func (c *Client) EnsureReady(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx != nil {
		return nil
	}
	chrome := c.ResolveChromePath()
	if chrome == "" {
		return errors.New("solo: no Chrome or Chromium binary found on PATH or in standard locations")
	}
	userData := c.cfg.UserDataDir
	if userData == "" {
		d, err := os.MkdirTemp("", "bashy-browser-solo-*")
		if err != nil {
			return fmt.Errorf("solo: temp user-data-dir: %w", err)
		}
		userData = d
		c.tempUserData = d
	} else if err := os.MkdirAll(userData, 0o755); err != nil {
		return fmt.Errorf("solo: user-data-dir: %w", err)
	}

	opts := chromedp.DefaultExecAllocatorOptions[:]
	opts = append(opts,
		chromedp.ExecPath(chrome),
		chromedp.UserDataDir(userData),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
	)
	if c.cfg.Headed {
		opts = append(opts, chromedp.Flag("headless", false))
	} else {
		// --headless=new, NOT chromedp.Headless (which emits the bare
		// --headless == old headless). Chrome 132+ removed old headless and
		// IGNORES the bare flag, so the browser boots HEADED and then aborts in
		// _RegisterApplication (SIGABRT) when it can't reach the WindowServer
		// from a non-GUI context (CI, a fleet worker, ssh) — firing a crash
		// report each time. New headless never registers as a GUI app.
		opts = append(opts, chromedp.Flag("headless", "new"))
	}
	allocCtx, allocStop := chromedp.NewExecAllocator(context.Background(), opts...)
	cdpCtx, cdpStop := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(cdpCtx); err != nil {
		cdpStop()
		allocStop()
		if c.tempUserData != "" {
			_ = os.RemoveAll(c.tempUserData)
			c.tempUserData = ""
		}
		return fmt.Errorf("solo: launch %s: %w", chrome, err)
	}
	c.allocCtx, c.allocStop = allocCtx, allocStop
	c.ctx, c.ctxStop = cdpCtx, cdpStop
	return nil
}

func (c *Client) Execute(ctx context.Context, action wire.Action) (*wire.Result, error) {
	c.mu.Lock()
	cdpCtx := c.ctx
	c.mu.Unlock()
	if cdpCtx == nil {
		if err := c.EnsureReady(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
		cdpCtx = c.ctx
		c.mu.Unlock()
	}
	if cdpCtx == nil {
		return nil, errors.New("solo: not ready")
	}
	callCtx, cancel := context.WithTimeout(cdpCtx, 30*time.Second)
	defer cancel()
	return cdpactions.Run(callCtx, Mode, action)
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	if c.tempUserData != "" {
		_ = os.RemoveAll(c.tempUserData)
		c.tempUserData = ""
	}
	return nil
}

func DefaultUserDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "bashy", "browser-solo")
}
