package bus

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

type onceReceipt struct {
	Fingerprint string `json:"fingerprint"`
	Seq         int64  `json:"seq"`
	Complete    bool   `json:"complete"`
}

var postOnceAfterAppend func() error // deterministic append-before-receipt crash seam

// PostMessageOnce uses the board's existing delivery/cursor protocol and a
// durable key receipt. A crash after append is recovered from the keyed post,
// including rotated archives. Uncertain recovery fails without a duplicate.
// It never falls back to an unlocked append. Recovery is capped at 64MiB and
// 128 archive files; exceeding the bound leaves the notice pending for repair.
func PostMessageOnce(ctx context.Context, key string, p Post) (int64, error) {
	if strings.TrimSpace(key) == "" || len(key) > 256 || strings.TrimSpace(p.From) == "" || strings.TrimSpace(p.To) == "" {
		return 0, fmt.Errorf("idempotent board notice requires bounded key, sender and recipient")
	}
	p.Seq = 0
	p.At = ""
	p.SchemaVersion = BoardSchema
	p.IdempotencyKey = key
	b, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	if len(b) > 8192 {
		return 0, fmt.Errorf("idempotent notice exceeds 8KiB")
	}
	fingerprint := sha256.Sum256(b)
	keyHash := sha256.Sum256([]byte(key))
	dir := filepath.Join(BoardDir(), "receipts")
	if BoardDir() == "" {
		return 0, fmt.Errorf("board directory unavailable")
	}
	path := filepath.Join(dir, hex.EncodeToString(keyHash[:])+".json")
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var lock *lockfile.Lock
	for {
		if err := lockCtx.Err(); err != nil {
			return 0, err
		}
		lock, err = lockfile.TryAcquire(boardLockPath(), lockfile.Holder{Name: "board-once", Intent: "notice"})
		if err == nil {
			break
		}
		if !errors.Is(err, lockfile.ErrHeld) {
			return 0, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-lockCtx.Done():
			timer.Stop()
			return 0, lockCtx.Err()
		case <-timer.C:
		}
	}
	defer func() { lock.Release(); rotateBoardOpportunistic() }()
	var receipt onceReceipt
	var data []byte
	receiptFile, readErr := os.Open(path)
	if readErr == nil {
		data, readErr = io.ReadAll(io.LimitReader(receiptFile, 4097))
		receiptFile.Close()
		if len(data) > 4096 {
			return 0, fmt.Errorf("notice receipt exceeds 4KiB")
		}
	}
	if readErr == nil {
		if err := json.Unmarshal(data, &receipt); err != nil {
			return 0, fmt.Errorf("notice receipt corrupt: %w", err)
		}
		if receipt.Fingerprint != hex.EncodeToString(fingerprint[:]) {
			return 0, fmt.Errorf("notice key reused with different content")
		}
		if receipt.Complete {
			return receipt.Seq, nil
		}
	} else if !os.IsNotExist(readErr) {
		return 0, readErr
	}
	if err := terminateBoardPartial(); err != nil {
		return 0, err
	}
	if readErr == nil {
		seq, err := recoverNotice(ctx, key)
		if err != nil {
			return 0, err
		}
		if seq > 0 {
			receipt.Seq = seq
			receipt.Complete = true
			return seq, writeOnceReceipt(path, receipt)
		}
		if archivedThrough() >= receipt.Seq {
			return 0, fmt.Errorf("notice recovery uncertain after rotation; receipt remains pending")
		}
	}
	// Sequence assignment follows existing valid-record plus archive-base rules.
	info, err := os.Stat(postsPath())
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if info != nil && info.Size() > 64<<20 {
		return 0, fmt.Errorf("board exceeds bounded notice append scan")
	}
	posts, err := Posts()
	if err != nil {
		return 0, err
	}
	p.Seq = archivedThrough() + int64(len(posts)) + 1
	p.At = time.Now().UTC().Format(time.RFC3339)
	receipt = onceReceipt{Fingerprint: hex.EncodeToString(fingerprint[:]), Seq: p.Seq}
	if err := writeOnceReceipt(path, receipt); err != nil {
		return 0, err
	}
	b, err = json.Marshal(p)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(postsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if postOnceAfterAppend != nil {
		if err := postOnceAfterAppend(); err != nil {
			return 0, err
		}
	}
	receipt.Complete = true
	return p.Seq, writeOnceReceipt(path, receipt)
}
func terminateBoardPartial() error {
	f, err := os.OpenFile(postsPath(), os.O_RDWR, 0600)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	var last [1]byte
	if _, err = f.ReadAt(last[:], info.Size()-1); err != nil {
		return err
	}
	if last[0] != '\n' {
		_, err = f.WriteAt([]byte{'\n'}, info.Size())
	}
	return err
}
func writeOnceReceipt(path string, r onceReceipt) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}
func recoverNotice(ctx context.Context, key string) (int64, error) {
	paths := []string{postsPath()}
	dir, err := os.Open(archiveDir())
	if err == nil {
		entries, e := dir.ReadDir(129)
		dir.Close()
		if e != nil && e != io.EOF {
			return 0, e
		}
		if len(entries) > 128 {
			return 0, fmt.Errorf("notice archive recovery file limit exceeded")
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".jsonl") {
				paths = append(paths, filepath.Join(archiveDir(), entry.Name()))
			}
		}
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	remaining := int64(64 << 20)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		remaining -= info.Size()
		if remaining < 0 {
			f.Close()
			return 0, fmt.Errorf("notice recovery byte limit exceeded")
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 8192), 1<<20)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				f.Close()
				return 0, err
			}
			var p Post
			if json.Unmarshal(scanner.Bytes(), &p) == nil && p.IdempotencyKey == key {
				f.Close()
				return p.Seq, nil
			}
		}
		err = scanner.Err()
		f.Close()
		if err != nil {
			return 0, err
		}
	}
	return 0, nil
}
