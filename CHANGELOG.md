# Changelog

- [#60](https://github.com/crypto-org-chain/cronos-store/pull/60) fix(versiondb): propagate readSnapshotEntries error in restore-versiondb.
- [#56](https://github.com/crypto-org-chain/cronos-store/pull/56) fix(versiondb): fix use-after-free in tsrocksdb iterator — ReadOptions must outlive the iterator to prevent dangling pointer in DBIter::timestamp_ub_.
- [#55](https://github.com/crypto-org-chain/cronos-store/pull/55) fix(versiondb): ignore non-leaf nodes data in restore-versiondb cli command.
- [#54](https://github.com/crypto-org-chain/cronos-store/pull/54) fix(store): close memiavl db loaded in CacheMultiStoreWithVersion.
- [#47](https://github.com/crypto-org-chain/cronos-store/pull/47) feat(cosmos-sdk): Optimize staking end-block queue through using pending queue slots instead of iterators. 
- [#45](https://github.com/crypto-org-chain/cronos-store/pull/45) feat: use rocksdb v10.9.1.
- [#41](https://github.com/crypto-org-chain/cronos-store/pull/41) fix: update cosmos-sdk with signature incarnation cache removed for SigVerificationDecorator
- [#40](https://github.com/crypto-org-chain/cronos-store/pull/40) fix: update cosmos-sdk with pre-estimate bug fixed
- [#34](https://github.com/crypto-org-chain/cronos-store/pull/34) feat: use rocksdb v10.6.2.
- [#38](https://github.com/crypto-org-chain/cronos-store/pull/38) feat: upgrade to cosmos-sdk v0.53.4
- [#22](https://github.com/crypto-org-chain/cronos-store/pull/22) feat: use thread safe cache.
- [#23](https://github.com/crypto-org-chain/cronos-store/pull/23) perf(memiavl): optimize DB.ApplyChangeSet with pending map cache.
- [#10](https://github.com/crypto-org-chain/cronos-store/pull/10) feat: use rocksdb v10.5.1.
- [#24](https://github.com/crypto-org-chain/cronos-store/pull/24) feat(memiavl): add CLI command to dump memiavl changeset.
- [#11](https://github.com/crypto-org-chain/cronos-store/pull/11) fix(memiavl): clone key/values in tree.Get/tree.set when zerocopy is disabled.
- [#6](https://github.com/crypto-org-chain/cronos-store/pull/6) feat(memiavl/client): add CLI command to dump the memiavl root.
- [#7](https://github.com/crypto-org-chain/cronos-store/pull/7) fix: memiavl WriteSnapshotWithContext cancel using wrong ctx.
- [#4](https://github.com/crypto-org-chain/cronos-store/pull/4) feat(versiondb/client): add dump versiondb changeset cmd.
- [#3](https://github.com/crypto-org-chain/cronos-store/pull/3) feat(memiavl): MultiTree add chainId.
- [#1](https://github.com/crypto-org-chain/cronos-store/pull/1) feature: add store, memiavl, versiondb.

