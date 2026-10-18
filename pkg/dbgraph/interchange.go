package dbgraph

import (
	"context"
	"fmt"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/bonsaidb"
)

// Interchange requires a full graph scan in both adapters. The MemGraph
// GraphTx interface currently exposes only identity lookup and adjacency;
// this category refuses an incomplete export rather than silently dropping
// isolated nodes.
func runExport(context.Context, *bonsaidb.DB, Input) (dblifecycle.Outcome, error) {
	return dblifecycle.Outcome{}, fmt.Errorf("%w: graph enumeration is required for lossless export", dbstore.ErrUnsupported)
}

func runImport(context.Context, *bonsaidb.DB, Input) (dblifecycle.Outcome, error) {
	return dblifecycle.Outcome{}, fmt.Errorf("%w: graph enumeration is required for lossless import", dbstore.ErrUnsupported)
}
