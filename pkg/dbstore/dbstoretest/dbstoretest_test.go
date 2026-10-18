package dbstoretest

import (
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
)

func TestMemKVConformance(t *testing.T) { RunKVConformance(t, dbstore.NewMemKV()) }
