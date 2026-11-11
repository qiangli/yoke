// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package qrterm renders a payload as a QR code: half-block characters for a
// terminal, or a PNG file. It is the one QR seam in the userland, shared by
// `app pair` (the console pairing ticket) and `share qr` (any URL).
package qrterm

import (
	"fmt"
	"os"

	qrcode "github.com/skip2/go-qrcode"
)

// Terminal renders payload as a QR block of half-block characters, with the
// quiet-zone border the scanner needs. The block ends with a newline.
func Terminal(payload string) (string, error) {
	q, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return "", err
	}
	q.DisableBorder = false
	return q.ToSmallString(false), nil
}

// PNG writes payload as a size x size pixel PNG at path (0644). A size of 0
// or less takes the 256-pixel default, enough for a phone camera at arm's
// length.
func PNG(payload, path string, size int) error {
	if size <= 0 {
		size = 256
	}
	png, err := qrcode.Encode(payload, qrcode.Medium, size)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return fmt.Errorf("qrterm: write %s: %w", path, err)
	}
	return nil
}
