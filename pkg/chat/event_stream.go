package chat

import (
	"bytes"
	"context"
	"io"
	"os"
	"time"
)

// streamEventFile bridges a tool's append-only NDJSON side-channel to the live
// observer stream. It advances only through complete lines, so a cancellation
// racing the writer cannot expose a torn JSON event.
func streamEventFile(ctx context.Context, path string, out io.Writer, done chan<- struct{}) {
	defer close(done)
	if out == nil {
		return
	}
	var offset int64
	drain := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
		buf, err := io.ReadAll(f)
		if err != nil {
			return
		}
		nl := bytes.LastIndexByte(buf, '\n')
		if nl < 0 {
			return
		}
		complete := buf[:nl+1]
		if _, err := out.Write(complete); err == nil {
			offset += int64(len(complete))
		}
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			drain()
			return
		case <-tick.C:
			drain()
		}
	}
}
