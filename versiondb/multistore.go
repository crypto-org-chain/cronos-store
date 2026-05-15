package versiondb

import (
	"fmt"

	"github.com/cosmos/cosmos-sdk/store/v2/cachemulti"
	"github.com/cosmos/cosmos-sdk/store/v2/types"
)

var _ types.MultiStore = (*MultiStore)(nil)

// MultiStore wraps `VersionStore` to implement `MultiStore` interface.
type MultiStore struct {
	versionDB VersionStore
	stores    map[types.StoreKey]types.KVStore

	// transient/memory/object stores, they are delegated to the parent
	delegatedStoreKeys map[types.StoreKey]struct{}

	// proxy the calls for transient or mem stores to the parent
	parent types.MultiStore
}

// NewMultiStore returns a new versiondb `MultiStore`.
func NewMultiStore(
	parent types.MultiStore,
	versionDB VersionStore,
	storeKeys map[string]*types.KVStoreKey,
	delegatedStoreKeys map[types.StoreKey]struct{},
) *MultiStore {
	stores := make(map[types.StoreKey]types.KVStore, len(storeKeys))
	for _, k := range storeKeys {
		stores[k] = NewKVStore(versionDB, k.Name(), nil)
	}
	return &MultiStore{
		versionDB:          versionDB,
		stores:             stores,
		parent:             parent,
		delegatedStoreKeys: delegatedStoreKeys,
	}
}

// GetStoreType implements `MultiStore` interface.
func (s *MultiStore) GetStoreType() types.StoreType {
	return types.StoreTypeMulti
}

// cacheMultiStore branch out the multistore.
func (s *MultiStore) cacheMultiStore(version *int64) types.CacheMultiStore {
	stores := make(map[types.StoreKey]types.CacheWrapper, len(s.delegatedStoreKeys)+len(s.stores))
	for k := range s.delegatedStoreKeys {
		stores[k] = types.CacheWrapper(s.parent.GetStore(k))
	}
	for k := range s.stores {
		if version == nil {
			stores[k] = s.stores[k]
		} else {
			stores[k] = NewKVStore(s.versionDB, k.Name(), version)
		}
	}
	return cachemulti.NewStore(stores)
}

// CacheMultiStore implements `MultiStore` interface
func (s *MultiStore) CacheMultiStore() types.CacheMultiStore {
	return s.cacheMultiStore(nil)
}

// CacheMultiStoreWithVersion implements `MultiStore` interface
func (s *MultiStore) CacheMultiStoreWithVersion(version int64) (types.CacheMultiStore, error) {
	return s.cacheMultiStore(&version), nil
}

// CacheWrap implements CacheWrapper/MultiStore/CommitStore.
func (s *MultiStore) CacheWrap() types.CacheWrap {
	return s.CacheMultiStore().(types.CacheWrap)
}

// GetStore implements `MultiStore` interface
func (s *MultiStore) GetStore(storeKey types.StoreKey) types.Store {
	if store, ok := s.stores[storeKey]; ok {
		return store
	}
	if _, ok := s.delegatedStoreKeys[storeKey]; ok {
		// delegate the transient/memory/object stores to real cms
		return s.parent.GetStore(storeKey)
	}
	panic(fmt.Errorf("store key %s is not registered", storeKey.Name()))
}

// GetKVStore implements `MultiStore` interface
func (s *MultiStore) GetKVStore(storeKey types.StoreKey) types.KVStore {
	return s.GetStore(storeKey).(types.KVStore)
}

// SetTracer implements `MultiStore` interface.
func (s *MultiStore) SetTracer(_ interface{}) types.MultiStore {
	return s
}

// SetTracingContext implements `MultiStore` interface.
func (s *MultiStore) SetTracingContext(_ interface{}) types.MultiStore {
	return s
}

// TracingEnabled returns if tracing is enabled for the MultiStore.
func (s *MultiStore) TracingEnabled() bool {
	return false
}

// LatestVersion returns the latest version saved in versiondb
func (s *MultiStore) LatestVersion() int64 {
	version, err := s.versionDB.GetLatestVersion()
	if err != nil {
		panic(err)
	}
	return version
}

// Close will flush the versiondb
func (s *MultiStore) Close() error {
	return s.versionDB.Flush()
}
