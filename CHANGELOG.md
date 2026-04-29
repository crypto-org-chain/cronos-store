# Changelog

- [#53](https://github.com/crypto-org-chain/cronos-store/pull/53) fix(store): close memiavl db loaded in CacheMultiStoreWithVersion.
- [#24](https://github.com/crypto-org-chain/cronos-store/pull/24) feat(memiavl): add CLI command to dump memiavl changeset.
- [#23](https://github.com/crypto-org-chain/cronos-store/pull/23) perf(memiavl): optimize DB.ApplyChangeSet with pending map cache.
- [#11](https://github.com/crypto-org-chain/cronos-store/pull/11) fix(memiavl): clone key/values in tree.Get/tree.set when zerocopy is disabled.
- [#10](https://github.com/crypto-org-chain/cronos-store/pull/10) feat: use rocksdb v10.5.1.
- [#7](https://github.com/crypto-org-chain/cronos-store/pull/7) fix: memiavl WriteSnapshotWithContext cancel using wrong ctx.
- [#6](https://github.com/crypto-org-chain/cronos-store/pull/6) feat(memiavl/client): add CLI command to dump the memiavl root.
- [#4](https://github.com/crypto-org-chain/cronos-store/pull/4) feat(versiondb/client): add dump versiondb changeset cmd.
- [#3](https://github.com/crypto-org-chain/cronos-store/pull/3) feat(memiavl): MultiTree add chainId.
- [#1](https://github.com/crypto-org-chain/cronos-store/pull/1) feature: add store, memiavl, versiondb.
