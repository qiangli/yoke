// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package share is the `share` front door: hand a link to someone on another
// device. `share qr URL` prints the link and a terminal QR code for ANY URL;
// no bashy account, satellite or outpost is involved in producing it.
package share

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/qiangli/yoke/pkg/qrterm"
	"github.com/spf13/cobra"
)

// NewCmd returns the `share` command tree.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "hand a link to a phone or browser user: print it with a QR code",
		Long: `share hands a link to someone on another device.

It works for ANY URL, from any bashy user: an app page, a document, a
satellite console, a plain web page. Nothing is published or changed by
sharing; the link only reaches whoever can already open it. When the URL is
a bashy app page, what opens on the phone is the owner's shapeable app page
as it stands at that moment.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newQRCmd())
	return cmd
}

func newQRCmd() *cobra.Command {
	var pngPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "qr URL [--png FILE] [--json]",
		Short: "print a URL and a terminal QR code for it (any URL)",
		Long: `qr prints the URL on the first line and a QR code for it below, so a
phone's camera opens the same link the terminal shows. Any URL with a scheme
and a host is accepted; nothing is fetched. A bashy app URL opens as the
owner's shapeable app page.

  --png FILE   also write the code as a PNG file (for a slide, a chat, a print)
  --json       print {"url":..., "png":...} instead of the terminal code`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := ValidateURL(args[0])
			if err != nil {
				return err
			}
			if pngPath != "" {
				if err := qrterm.PNG(u, pngPath, 0); err != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				return enc.Encode(struct {
					URL string `json:"url"`
					PNG string `json:"png"`
				}{URL: u, PNG: pngPath})
			}
			block, err := qrterm.Terminal(u)
			if err != nil {
				return fmt.Errorf("share qr: %w", err)
			}
			fmt.Fprintln(out, u)
			fmt.Fprint(out, block)
			if pngPath != "" {
				fmt.Fprintf(out, "png: %s\n", pngPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pngPath, "png", "", "also write the QR code as a PNG file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print {\"url\":..., \"png\":...} instead of the terminal code")
	return cmd
}

// ValidateURL accepts an absolute URL with a scheme and a host and returns it
// trimmed. Anything else is refused with a message that says what is missing,
// so a pasted fragment never becomes an unscannable or misleading code.
func ValidateURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("share qr: a URL is required (for example https://example.org/page)")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("share qr: not a URL: %q: %v", s, err)
	}
	if u.Scheme == "" {
		return "", fmt.Errorf("share qr: %q has no scheme; give the full link, such as https://%s", s, s)
	}
	if u.Host == "" {
		return "", fmt.Errorf("share qr: %q has no host; give the full link, such as https://host/path", s)
	}
	return s, nil
}
