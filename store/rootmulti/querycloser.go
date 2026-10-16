package rootmulti

import (
	"io"
	"runtime"
	"sync"

	"github.com/crypto-org-chain/cronos-store/store/memiavlstore"

	log "cosmossdk.io/log/v2"

	"github.com/cosmos/cosmos-sdk/store/v2/cachekv"
	"github.com/cosmos/cosmos-sdk/store/v2/types"
)

// queryCloser releases a query store's db reference once, on Close. As a safety
// net for SDK paths that skip Close (an out-of-gas gRPC query before
// crypto-org-chain/cosmos-sdk#1849), it also releases it once the store becomes
// unreachable, and logs that it had to.
type queryCloser struct {
	release *releaseOnce
}

type releaseOnce struct {
	once sync.Once
	fn   func() error
}

func (r *releaseOnce) do() (released bool, err error) {
	r.once.Do(func() {
		released = true
		err = r.fn()
	})
	return released, err
}

func newQueryCloser(release func() error, logger log.Logger) *queryCloser {
	c := &queryCloser{release: &releaseOnce{fn: release}}
	runtime.AddCleanup(c, func(r *releaseOnce) {
		if released, err := r.do(); released {
			logger.Error("query store was not closed; released its db reference", "err", err)
		}
	}, c.release)
	return c
}

func (c *queryCloser) Close() error {
	_, err := c.release.do()
	return err
}

// pinnedStore keeps its query closer reachable while the store or any iterator
// from it is, so the closer's safety net can't release a db that is still read.
type pinnedStore struct {
	*memiavlstore.Store
	pin *queryCloser
}

func (s pinnedStore) CacheWrap() types.CacheWrap {
	return cachekv.NewStore(s)
}

func (s pinnedStore) CacheWrapWithTrace(_ io.Writer, _ interface{}) types.CacheWrap {
	return cachekv.NewStore(s)
}

func (s pinnedStore) Iterator(start, end []byte) types.Iterator {
	return pinnedIterator{Iterator: s.Store.Iterator(start, end), pin: s.pin}
}

func (s pinnedStore) ReverseIterator(start, end []byte) types.Iterator {
	return pinnedIterator{Iterator: s.Store.ReverseIterator(start, end), pin: s.pin}
}

type pinnedIterator struct {
	types.Iterator
	pin *queryCloser
}
