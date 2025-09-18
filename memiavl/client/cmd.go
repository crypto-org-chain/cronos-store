package client

import (
	"cosmossdk.io/store/types"
	"fmt"

	"github.com/crypto-org-chain/cronos-store/memiavl"
	"github.com/spf13/cobra"
)

const (
	flagVersion = "version"
	flagChainId = "chain-id"
)

// GetCmd returns the root command for MemIAVL CLI utilities.
func DumpRootCmd(storeNames []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memiavl",
		Short: "memiavl cmds",
	}

	cmd.AddCommand(
		dumpRootCmd(storeNames),
	)

	return cmd
}

// getDumpRootCmd returns the command to dump MemIAVL root hashes.
func dumpRootCmd(storeNames []string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dump-memiavl-root",
		Short: "dump memiavl root at version [dir]",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			version, err := cmd.Flags().GetUint32(flagVersion)
			if err != nil {
				return err
			}

			opts := memiavl.Options{
				InitialStores:   storeNames,
				CreateIfMissing: false,
				TargetVersion:   version,
			}

			db, err := memiavl.Load(dir, opts, flagChainId)
			if err != nil {
				return fmt.Errorf("failed to load MemIAVL DB: %w", err)
			}
			defer db.Close()

			// Dump per-module tree roots
			for _, storeName := range storeNames {
				tree := db.TreeByName(storeName)
				if tree != nil {
					fmt.Printf("module %s version %d RootHash %X\n", storeName, tree.Version(), tree.RootHash())
				} else {
					fmt.Printf("module %s not loaded\n", storeName)
				}
			}

			db.MultiTree.UpdateCommitInfo()
			lastCommitInfo := convertCommitInfo(db.MultiTree.LastCommitInfo())
			fmt.Printf("MultiTree Version %d RootHash %X\n", lastCommitInfo.Version, lastCommitInfo.Hash())

			return nil
		},
	}

	cmd.Flags().Uint32(flagVersion, 0, "Target version to load")
	cmd.Flags().String(flagChainId, "", "specify the chain id")

	return cmd
}

func convertCommitInfo(commitInfo *memiavl.CommitInfo) *types.CommitInfo {
	storeInfos := make([]types.StoreInfo, len(commitInfo.StoreInfos))
	for i, storeInfo := range commitInfo.StoreInfos {
		storeInfos[i] = types.StoreInfo{
			Name: storeInfo.Name,
			CommitId: types.CommitID{
				Version: storeInfo.CommitId.Version,
				Hash:    storeInfo.CommitId.Hash,
			},
		}
	}
	return &types.CommitInfo{
		Version:    commitInfo.Version,
		StoreInfos: storeInfos,
	}
}
