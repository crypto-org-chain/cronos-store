package client

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"

	"github.com/alitto/pond"
	"github.com/cosmos/gogoproto/jsonpb"
	"github.com/cosmos/iavl"
	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/spf13/cobra"

	storetypes "github.com/cosmos/cosmos-sdk/store/v2/types"
)

func VerifyChangeSetCmd(defaultStores []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify changeSetDir",
		Short: "Replay the input change set files in order to rebuild iavl tree in memory and output app hash and full json encoded commit info, user can compare the root hash against the block headers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			concurrency, err := cmd.Flags().GetInt(flagConcurrency)
			if err != nil {
				return err
			}
			targetVersion, err := cmd.Flags().GetInt64(flagTargetVersion)
			if err != nil {
				return err
			}
			saveSnapshot, err := cmd.Flags().GetString(flagSaveSnapshot)
			if err != nil {
				return err
			}
			loadSnapshot, err := cmd.Flags().GetString(flagLoadSnapshot)
			if err != nil {
				return err
			}
			check, err := cmd.Flags().GetBool(flagCheck)
			if err != nil {
				return err
			}
			save, err := cmd.Flags().GetBool(flagSave)
			if err != nil {
				return err
			}
			stores, err := GetStoresOrDefault(cmd, defaultStores)
			if err != nil {
				return err
			}

			chainId, err := cmd.Flags().GetString(flagChainId)
			if err != nil {
				return err
			}

			if len(saveSnapshot) > 0 {
				// detect the write permission early on.
				if err := os.MkdirAll(saveSnapshot, os.ModePerm); err != nil {
					return err
				}
			}

			changeSetDir := args[0]

			// Registered before the pool's StopAndWait so it runs after it (defers are
			// LIFO): with --load-snapshot the trees read that snapshot's mmap zero-copy,
			// so the mapping has to outlive every worker touching them.
			var mtree *memiavl.MultiTree
			defer func() {
				if mtree != nil {
					_ = mtree.Close()
				}
			}()

			// create fixed size task pool with big enough buffer.
			pool := pond.New(concurrency, 0)
			defer pool.StopAndWait()

			mtree = memiavl.NewEmptyMultiTree(0, 0, chainId)
			if len(loadSnapshot) > 0 {
				mtree, err = memiavl.LoadMultiTree(loadSnapshot, true, 0, chainId)
				if err != nil {
					return err
				}
			}

			// A repeated name would otherwise hand the same tree to two workers, which
			// then replay change sets into it concurrently and corrupt it silently.
			stores = dedupStores(stores)

			verified := make([]verifiedStore, len(stores))
			err = memiavl.RunWorkerGroup(pool, stores, func(i int) error {
				store := stores[i]
				tree := mtree.TreeByName(store)
				// A store loaded from --load-snapshot exists even with no change sets to
				// replay; dropping it would shrink the store set, changing the app hash and
				// leaving the written snapshot's metadata inconsistent with its own trees.
				fromSnapshot := tree != nil
				if tree == nil {
					tree = memiavl.New(0)
				}
				exists, err := verifyOneStore(tree, store, changeSetDir, targetVersion)
				if err != nil {
					return err
				}
				if !exists && !fromSnapshot {
					// the store don't exist before target version, don't affect the commit info and app hash.
					return nil
				}
				verified[i] = verifiedStore{name: store, tree: tree}
				return nil
			})
			if err != nil {
				return err
			}

			verified = slices.DeleteFunc(verified, func(entry verifiedStore) bool {
				return entry.tree == nil
			})

			// All stores must end on the same version: a multitree snapshot records one
			// commit-info version for the whole set and rejects trees that disagree with
			// it on load. With --target-version unset every store stops at its own last
			// changeset, so bump the laggards here the way the live path does each block.
			// Only known once every store has been replayed, hence after Wait.
			lastestVersion := targetVersion
			for _, entry := range verified {
				if v := entry.tree.Version(); v > lastestVersion {
					lastestVersion = v
				}
			}

			storeInfos := make([]storetypes.StoreInfo, 0, len(verified))
			for _, entry := range verified {
				if err := advanceTreeVersion(entry.tree, lastestVersion); err != nil {
					return err
				}
				storeInfos = append(storeInfos, storetypes.StoreInfo{
					Name:     entry.name,
					CommitId: lastCommitID(entry.tree),
				})
			}

			commitInfo := buildCommitInfo(storeInfos, lastestVersion)

			if len(saveSnapshot) > 0 {
				names := make([]string, len(verified))
				for i, entry := range verified {
					names[i] = entry.name
				}
				if err := memiavl.RunWorkerGroup(pool, names, func(i int) error {
					entry := verified[i]
					return entry.tree.WriteSnapshot(filepath.Join(saveSnapshot, entry.name))
				}); err != nil {
					return err
				}

				// Written through the multitree so the metadata carries its initial
				// version too: loadMultiTree derives the version it expects the trees
				// to be at from that field.
				if err := mtree.WriteMetadata(saveSnapshot, convertCommitInfo(&commitInfo)); err != nil {
					return err
				}
			}

			// write out the replay result
			var buf bytes.Buffer
			buf.WriteString(hex.EncodeToString(commitInfo.Hash()))
			buf.WriteString("\n")
			marshaler := jsonpb.Marshaler{}
			if err := marshaler.Marshal(&buf, &commitInfo); err != nil {
				return err
			}

			verifiedFileName := filepath.Join(changeSetDir, fmt.Sprintf("verified-%d", commitInfo.Version))
			if check {
				// check commitInfo against the one stored in change set
				bz, err := os.ReadFile(verifiedFileName)
				if err != nil {
					return err
				}

				if !bytes.Equal(buf.Bytes(), bz) {
					return fmt.Errorf("verify result don't match")
				}

				fmt.Printf("version %d checked successfully\n", commitInfo.Version)
				return nil
			}

			if save {
				if err := os.WriteFile(verifiedFileName, buf.Bytes(), 0o600); err != nil {
					return err
				}
				fmt.Printf("version %d verify result saved to %s\n", commitInfo.Version, verifiedFileName)
				return nil
			}

			_, err = os.Stdout.Write(buf.Bytes())
			return err
		},
	}

	cmd.Flags().Int64(flagTargetVersion, 0, "specify the target version, otherwise it'll exhaust the plain files")
	cmd.Flags().String(flagStores, "", "list of store names, default to the current store list in application")
	cmd.Flags().String(flagSaveSnapshot, "", "save the snapshot of the target iavl tree to directory")
	cmd.Flags().String(flagLoadSnapshot, "", "load the snapshot before doing verification from directory")
	cmd.Flags().Int(flagConcurrency, runtime.NumCPU(), "Number concurrent goroutines to parallelize the work")
	cmd.Flags().Bool(flagCheck, false, "Check the replayed hash with the one stored in change set directory")
	cmd.Flags().Bool(flagSave, false, "Save the verify result to change set directory, otherwise output to stdout")
	cmd.Flags().String(flagChainId, "", "specify the chain id")

	return cmd
}

// verifiedStore pairs a replayed tree with its store name; the tree is still open so its
// version can be bumped and its snapshot written once the final version is known.
type verifiedStore struct {
	name string
	tree *memiavl.Tree
}

// verifyOneStore is safe to run in parallel with other stores. Reports false without
// error if the store doesn't exist before `targetVersion`.
func verifyOneStore(tree *memiavl.Tree, store, changeSetDir string, targetVersion int64) (bool, error) {
	filesWithVersion, err := scanChangeSetFiles(changeSetDir, store)
	if err != nil {
		return false, err
	}

	if len(filesWithVersion) == 0 {
		return false, nil
	}
	// set the initial version for the store
	initialVersion := filesWithVersion[0].Version
	if targetVersion > 0 && initialVersion > uint64(targetVersion) {
		return false, nil
	}

	if err := tree.SetInitialVersion(int64(initialVersion)); err != nil {
		return false, err
	}

	for _, file := range filesWithVersion {
		if targetVersion > 0 && file.Version > uint64(targetVersion) {
			break
		}

		err = withChangeSetFile(file.FileName, func(reader Reader) error {
			_, err := IterateChangeSets(reader, func(version int64, changeSet *iavl.ChangeSet) (bool, error) {
				if version <= tree.Version() {
					// skip old change sets
					return true, nil
				}

				// changesets only cover versions this store touched; bump through the gap so it
				// stays in lockstep with the live path, which advances every store every block.
				if err := advanceTreeVersion(tree, version-1); err != nil {
					return false, err
				}

				// no need to update hashes for intermediate versions.
				tree.ApplyChangeSet(convertChangeSet(changeSet))
				_, v, err := tree.SaveVersion(false)
				if err != nil {
					return false, err
				}
				if v != version {
					return false, fmt.Errorf("version don't match: %d != %d", v, version)
				}
				// gap-filling above still runs even when targetVersion == 0 (exhaust all files); this
				// only controls whether the loop stops early once targetVersion is reached.
				return targetVersion == 0 || v < targetVersion, nil
			})

			return err
		})
		if err != nil {
			break
		}

		if targetVersion > 0 && tree.Version() >= targetVersion {
			break
		}
	}

	if err != nil {
		return false, err
	}

	// no more changesets for this store; catch up to targetVersion like the live path would.
	if targetVersion > 0 {
		if err := advanceTreeVersion(tree, targetVersion); err != nil {
			return false, err
		}
	}

	return true, nil
}

// dedupStores builds a new slice rather than compacting in place: with --stores
// unset, GetStoresOrDefault hands back the caller's own defaultStores slice, and
// reordering plus tail-zeroing it would corrupt that shared value.
func dedupStores(stores []string) []string {
	seen := make(map[string]struct{}, len(stores))
	deduped := make([]string, 0, len(stores))
	for _, store := range stores {
		if _, ok := seen[store]; ok {
			continue
		}
		seen[store] = struct{}{}
		deduped = append(deduped, store)
	}
	return deduped
}

// advanceTreeVersion saves empty versions up to `target`, applying no changeset, so a
// store's version stays in lockstep with the live path, which advances every store's
// tree version every block regardless of whether it changed.
func advanceTreeVersion(tree *memiavl.Tree, target int64) error {
	for tree.Version() < target {
		if _, _, err := tree.SaveVersion(false); err != nil {
			return err
		}
	}
	return nil
}

// lastCommitID build `CommitID` from a memiavl tree.
func lastCommitID(tree *memiavl.Tree) storetypes.CommitID {
	// copy out the hash in case it's relied on mmap-ed file.
	var hash [memiavl.SizeHash]byte
	copy(hash[:], tree.RootHash())
	return storetypes.CommitID{
		Version: tree.Version(),
		Hash:    hash[:],
	}
}

// buildCommitInfo sort the storeInfos by store name, and built `CommitInfo`.
func buildCommitInfo(storeInfos []storetypes.StoreInfo, version int64) storetypes.CommitInfo {
	sort.SliceStable(storeInfos, func(i, j int) bool {
		return storeInfos[i].Name < storeInfos[j].Name
	})

	return storetypes.CommitInfo{
		Version:    version,
		StoreInfos: storeInfos,
	}
}

func convertCommitInfo(commitInfo *storetypes.CommitInfo) *memiavl.CommitInfo {
	storeInfos := make([]memiavl.StoreInfo, len(commitInfo.StoreInfos))
	for i, storeInfo := range commitInfo.StoreInfos {
		storeInfos[i] = memiavl.StoreInfo{
			Name: storeInfo.Name,
			CommitId: memiavl.CommitID{
				Version: storeInfo.CommitId.Version,
				Hash:    storeInfo.CommitId.Hash,
			},
		}
	}
	return &memiavl.CommitInfo{
		Version:    commitInfo.Version,
		StoreInfos: storeInfos,
	}
}

func convertChangeSet(cs *iavl.ChangeSet) memiavl.ChangeSet {
	pairs := make([]*memiavl.KVPair, len(cs.Pairs))
	for i, pair := range cs.Pairs {
		pairs[i] = &memiavl.KVPair{
			Delete: pair.Delete,
			Key:    pair.Key,
			Value:  pair.Value,
		}
	}
	return memiavl.ChangeSet{
		Pairs: pairs,
	}
}
