// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/prune"
)

// Pruner implements prune.Noun ("artifact") over the protected artifact store.
// DryRun never creates directories or mutates service_epoch; Apply rechecks
// protected closure, retained recipe inputs, and active leases before CAS removal.
type Pruner struct {
	Root string
}

var _ prune.Noun = Pruner{}

func (Pruner) Name() string { return "artifact" }

func (p Pruner) root() string {
	if s := strings.TrimSpace(p.Root); s != "" {
		return s
	}
	return DefaultRoot()
}

func (p Pruner) DryRun(ctx context.Context, opts prune.Options) ([]prune.Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := p.root()
	if client, ok := TryConnectService(ctx, root); ok {
		defer client.Close()
		return client.DryRunPrune(ctx, opts.Older)
	}
	dbPath := filepath.Join(root, "catalog.db")
	st, err := os.Stat(dbPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, nil
	}
	store, err := OpenStore(ctx, StoreOptions{Root: root, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.DryRunPrune(ctx, opts.Older)
}

func (p Pruner) Apply(ctx context.Context, items []prune.Item) (prune.Counts, error) {
	if err := ctx.Err(); err != nil {
		return prune.Counts{}, err
	}
	for _, item := range items {
		if item.Noun != p.Name() {
			return prune.Counts{}, fmt.Errorf("artifact prune: foreign item %q (%s)", item.ID, item.Noun)
		}
	}
	if len(items) == 0 {
		return prune.Counts{}, nil
	}
	root := p.root()
	if _, ok := TryConnectService(ctx, root); !ok {
		if _, err := os.Stat(filepath.Join(root, "catalog.db")); os.IsNotExist(err) {
			return prune.Counts{}, nil
		}
	}
	backend, err := ConnectOrOpen(ctx, root)
	if err != nil {
		return prune.Counts{}, err
	}
	defer backend.Close()
	return backend.ApplyPrune(ctx, items)
}
