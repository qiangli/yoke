package live

import "github.com/qiangli/yoke/pkg/browser"

// Service is a live-mode browser.Client backend.
var _ browser.Client = (*Service)(nil)
