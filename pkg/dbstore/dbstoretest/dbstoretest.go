// Package dbstoretest holds behavior suites every dbstore implementation of
// a shared interface must pass, so the persistent engine and its in-memory
// supplement are held to the same contract.
package dbstoretest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// RunKVConformance is the behavior shared by every KV implementation: atomic
// read-modify-write under concurrent writers (no lost updates), bucket
// lifecycle and ordered scans. Engine packages call it on their own KV.
func RunKVConformance(t *testing.T, kv dbstore.KV) {
	t.Helper()
	ctx := context.Background()
	b := []byte("counter")
	if err := kv.Update(ctx, func(tx dbstore.KVTx) error { return tx.CreateBucket(b) }); err != nil {
		t.Fatal(err)
	}
	if err := kv.Update(ctx, func(tx dbstore.KVTx) error { return tx.CreateBucket(b) }); !errors.Is(err, dbstore.ErrExists) {
		t.Fatalf("recreate bucket: %v", err)
	}
	const writers, each = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := kv.Update(ctx, func(tx dbstore.KVTx) error {
					v, _, err := tx.Get(b, []byte("n"))
					if err != nil {
						return err
					}
					n := 0
					fmt.Sscan(string(v), &n)
					return tx.Put(b, []byte("n"), []byte(fmt.Sprint(n+1)))
				}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	_ = kv.View(ctx, func(tx dbstore.KVTx) error {
		v, _, _ := tx.Get(b, []byte("n"))
		if string(v) != fmt.Sprint(writers*each) {
			t.Fatalf("lost updates: %s, want %d", v, writers*each)
		}
		return nil
	})
	if err := kv.Update(ctx, func(tx dbstore.KVTx) error {
		for _, k := range []string{"c", "a", "b"} {
			if err := tx.Put(b, []byte(k), []byte(k)); err != nil {
				return err
			}
		}
		return tx.Delete(b, []byte("n"))
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = kv.View(ctx, func(tx dbstore.KVTx) error {
		return tx.Scan(b, []byte("b"), func(k, v []byte) (bool, error) {
			got = append(got, string(k))
			return true, nil
		})
	})
	if strings.Join(got, ",") != "b,c" {
		t.Fatalf("scan %v", got)
	}
	if err := kv.Update(ctx, func(tx dbstore.KVTx) error { return tx.DeleteBucket(b) }); err != nil {
		t.Fatal(err)
	}
	_ = kv.View(ctx, func(tx dbstore.KVTx) error {
		bs, _ := tx.Buckets()
		if len(bs) != 0 {
			t.Fatalf("buckets after delete %q", bs)
		}
		return nil
	})
}
