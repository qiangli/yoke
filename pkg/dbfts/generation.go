package dbfts

import "github.com/qiangli/yoke/pkg/dbstore"

// Generation reports the index generation of the admitted handle's bleve
// index: the batch counter every OpPut, OpDelete and OpBatch commit bumps
// in the same batch as its writes. A memory.retrieve read brackets its
// index snapshot and ranked query with two Generation reads on the same
// handle; a mismatch means a writer committed mid-read — content ABA
// included, since the counter moves on every batch — and the read returns
// busy with no output instead of mixing generations. No new counter, lock
// registry or admission machinery: the counter is the one batch commits
// already maintain, read through the existing index open.
func Generation(h dbstore.Handle) (uint64, error) {
	d, err := index(h)
	if err != nil {
		return 0, err
	}
	return generation(d.Index())
}
