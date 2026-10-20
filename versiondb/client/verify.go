package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

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
			// a repeated or aliased name would hand one store's files, or loaded tree, to two workers.
			stores, err = normalizeStores(stores)
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

			mtree := memiavl.NewEmptyMultiTree(0, 0, chainId)
			if len(loadSnapshot) > 0 {
				if mtree, err = memiavl.LoadMultiTree(loadSnapshot, true, 0, chainId); err != nil {
					return err
				}
			}
			defer mtree.Close()
			// no change set can bring a tree back down to the target.
			if targetVersion > 0 && mtree.Version() > targetVersion {
				return fmt.Errorf("loaded snapshot version %d is newer than target version %d", mtree.Version(), targetVersion)
			}

			// create fixed size task pool.
			pool := pond.New(concurrency, 0)
			defer pool.StopAndWait()

			verified := make([]*storetypes.StoreInfo, len(stores))
			err = memiavl.RunWorkerGroup(cmd.Context(), pool, stores, func(ctx context.Context, i int) (err error) {
				if verified[i], err = replayStore(ctx, mtree, stores[i], changeSetDir, targetVersion, saveSnapshot); err != nil {
					return fmt.Errorf("store %s: %w", stores[i], err)
				}
				return nil
			})
			if err != nil {
				return err
			}

			storeInfos := make([]storetypes.StoreInfo, 0, len(verified))
			var latestVersion int64
			for _, info := range verified {
				if info != nil {
					storeInfos = append(storeInfos, *info)
					latestVersion = max(latestVersion, info.CommitId.Version)
				}
			}

			commitInfo := buildCommitInfo(storeInfos, latestVersion)

			if len(saveSnapshot) > 0 {
				// the output keeps each store's own version, but a multitree snapshot needs every tree at latestVersion.
				snapshotInfo := &memiavl.CommitInfo{Version: latestVersion, StoreInfos: make([]memiavl.StoreInfo, len(commitInfo.StoreInfos))}
				for i, info := range commitInfo.StoreInfos {
					if info.CommitId.Version < latestVersion {
						if err := memiavl.WriteSnapshotVersion(filepath.Join(saveSnapshot, info.Name), uint32(latestVersion)); err != nil {
							return err
						}
					}
					snapshotInfo.StoreInfos[i] = memiavl.StoreInfo{
						Name:     info.Name,
						CommitId: memiavl.CommitID{Version: latestVersion, Hash: info.CommitId.Hash},
					}
				}
				// keeps the loaded InitialVersion, which memiavl uses to map versions to WAL indexes.
				if err := mtree.WriteMetadata(saveSnapshot, snapshotInfo); err != nil {
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

// replayStore replays one store onto its loaded tree, or a new one, and closes the tree
// once it is hashed, and written to snapshotDir if set, so --concurrency bounds memory
// even for trees the loaded snapshot owns. It returns nil for a store absent at the target.
func replayStore(ctx context.Context, mtree *memiavl.MultiTree, store, changeSetDir string, targetVersion int64, snapshotDir string) (_ *storetypes.StoreInfo, err error) {
	tree := mtree.TreeByName(store)
	// a loaded store with no change sets still belongs in the app hash.
	fromSnapshot := tree != nil
	if tree == nil {
		tree = memiavl.New(0)
	}
	defer func() { err = errors.Join(err, tree.Close()) }()

	exists, err := verifyOneStore(ctx, tree, store, changeSetDir, targetVersion)
	if err != nil {
		return nil, err
	}
	if !exists && !fromSnapshot {
		// the store don't exist before target version, don't affect the commit info and app hash.
		return nil, nil
	}
	if targetVersion > 0 {
		// like the live path, every store reaches the target, including a loaded one without change sets.
		if err := advanceTreeVersion(tree, targetVersion); err != nil {
			return nil, err
		}
	}
	if snapshotDir != "" {
		if err := tree.WriteSnapshotWithContext(ctx, filepath.Join(snapshotDir, store)); err != nil {
			return nil, err
		}
	}
	return &storetypes.StoreInfo{Name: store, CommitId: lastCommitID(tree)}, nil
}

// verifyOneStore reports false without error if the store doesn't exist before targetVersion.
func verifyOneStore(ctx context.Context, tree *memiavl.Tree, store, changeSetDir string, targetVersion int64) (bool, error) {
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
				if err := ctx.Err(); err != nil {
					return false, err
				}
				if version <= tree.Version() {
					// skip old change sets
					return true, nil
				}
				if targetVersion > 0 && version > targetVersion {
					return false, nil
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

	return true, nil
}

// normalizeStores cleans names, drops repeats and rejects anything that isn't a single
// directory. It copies instead of compacting in place: GetStoresOrDefault may return the
// caller's shared defaultStores slice.
func normalizeStores(stores []string) ([]string, error) {
	seen := make(map[string]struct{}, len(stores))
	normalized := make([]string, 0, len(stores))
	for _, store := range stores {
		// a double space in --stores splits into an empty name.
		if store == "" {
			continue
		}
		name := filepath.Clean(store)
		if name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
			return nil, fmt.Errorf("invalid store name %q: must be a single directory name", store)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	if len(normalized) == 0 {
		return nil, errors.New("no store to verify")
	}
	return normalized, nil
}

// advanceTreeVersion saves empty versions up to target, as the live path bumps every store each block.
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
	// RootHash clones mmap-ed bytes, so the hash outlives the snapshot.
	return storetypes.CommitID{
		Version: tree.Version(),
		Hash:    tree.RootHash(),
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
