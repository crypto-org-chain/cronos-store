//go:build !objstore
// +build !objstore

package versiondb

import "github.com/cosmos/cosmos-sdk/store/v2/types"

// GetObjKVStore implements `MultiStore` interface
func (s *MultiStore) GetObjKVStore(storeKey types.StoreKey) types.ObjKVStore {
	panic("objstore is not supported")
}
