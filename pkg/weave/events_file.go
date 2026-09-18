package weave

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type weaveLockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *weaveLockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// followWeaveEventFile tails a tool-declared NDJSON file. The returned channel
// is consumed by agentpty's idle watchdog; readable summaries go to the normal
// worker log so operators see the same evidence that keeps the run alive.
func followWeaveEventFile(path string, log io.Writer) (<-chan struct{}, func()) {
	activity := make(chan struct{}, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var offset int
		consume := func() {
			data, err := os.ReadFile(path)
			if err != nil {
				return
			}
			if len(data) < offset {
				offset = 0
			}
			chunk := data[offset:]
			end := bytes.LastIndexByte(chunk, '\n')
			if end < 0 {
				return
			}
			for _, line := range bytes.Split(chunk[:end], []byte{'\n'}) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				select {
				case activity <- struct{}{}:
				default:
				}
				if log != nil {
					fmt.Fprintln(log, weaveDistillEventFileLine(line))
				}
			}
			offset += end + 1
		}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				consume() // do not lose a terminal event written just before exit
				return
			case <-ticker.C:
				consume()
			}
		}
	}()
	return activity, func() {
		close(stop)
		<-done
	}
}

func weaveDistillEventFileLine(line []byte) string {
	var event struct {
		Type string `json:"type"`
		Data struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &event); err != nil {
		return "[event] " + weaveTruncateLogText(strings.TrimSpace(string(line)), weaveStreamJSONTextLimit)
	}
	switch event.Type {
	case "turn.start":
		return "[event] turn started"
	case "tool.call":
		name := strings.TrimSpace(event.Data.Name)
		if name == "" {
			name = "tool"
		}
		return "[event] -> " + name
	case "turn.end":
		status := strings.TrimSpace(event.Data.Status)
		if status == "" {
			status = "unknown"
		}
		return "[event] turn ended status=" + status
	default:
		return "[event] " + strings.TrimSpace(event.Type)
	}
}
