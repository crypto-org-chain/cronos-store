package client

import (
	"fmt"
	"math"

	"github.com/crypto-org-chain/cronos-store/versiondb/tsrocksdb"
	"github.com/spf13/cobra"
)

func CompactVersionDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compact-versiondb versiondb-path",
		Short: "Rewrite all versiondb sst files with the current compression options, the node must be stopped",
		Long:  "Rewrite all versiondb sst files with the current compression options, e.g. recompress with ZSTD the LZ4 files left by ingest-versiondb-sst or the uncompressed files left by restore-versiondb. The node must be stopped, the db is locked while it runs.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rateLimit, err := cmd.Flags().GetUint64(flagRateLimit)
			if err != nil {
				return err
			}
			if rateLimit > math.MaxInt64>>20 {
				return fmt.Errorf("rate limit too large: %d MiB/s", rateLimit)
			}

			before, after, err := tsrocksdb.CompactVersionDB(args[0], int64(rateLimit)<<20)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sst files size: %d -> %d bytes\n", before, after)
			return nil
		},
	}
	cmd.Flags().Uint64(flagRateLimit, 0, "max flush and compaction write rate in MiB/s, protects other processes sharing the disk, 0 means unlimited")
	return cmd
}
