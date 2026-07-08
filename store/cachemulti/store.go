package cachemulti

import (
	"io"

	"github.com/cosmos/cosmos-sdk/store/v2/cachemulti"
	"github.com/cosmos/cosmos-sdk/store/v2/types"
)

var NoopCloser io.Closer = CloserFunc(func() error { return nil })

type CloserFunc func() error

func (fn CloserFunc) Close() error {
	return fn()
}

// Store wraps sdk's cachemulti.Store to add io.Closer interface
type Store struct {
	cachemulti.Store
	io.Closer
}

func NewStore(
	stores map[types.StoreKey]types.CacheWrapper,
	_ io.Writer, _ interface{},
	closer io.Closer,
) Store {
	if closer == nil {
		closer = NoopCloser
	}
	store := cachemulti.NewStore(stores)
	return Store{
		Store:  store,
		Closer: closer,
	}
}
