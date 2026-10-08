package memiavl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alitto/pond"
	"github.com/stretchr/testify/require"
)

const (
	store1Name = "store1"
	store2Name = "store2"
	store3Name = "store3"
	store4Name = "store4"
	store5Name = "store5"
)

func TestMultiTreeWriteSnapshotWithContextCancellation(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

	stores := []string{store1Name, store2Name, store3Name, store4Name, store5Name}
	var upgrades []*TreeNameUpgrade
	for _, name := range stores {
		upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
	}
	require.NoError(t, mtree.ApplyUpgrades(upgrades))

	for _, storeName := range stores {
		tree := mtree.TreeByName(storeName)
		require.NotNil(t, tree)

		for i := 0; i < 1000; i++ {
			tree.set([]byte(string(rune('a'+i%26))+string(rune('a'+(i/26)%26))), []byte("value"))
		}
	}

	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	pool := pond.New(2, 10)
	defer pool.StopAndWait()

	snapshotDir := t.TempDir()

	cancel()

	err = mtree.WriteSnapshotWithContext(ctx, snapshotDir, pool)

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

func TestMultiTreeWriteSnapshotWithTimeoutContext(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

	stores := []string{store1Name, store2Name, store3Name}
	var upgrades []*TreeNameUpgrade
	for _, name := range stores {
		upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
	}
	require.NoError(t, mtree.ApplyUpgrades(upgrades))

	for _, storeName := range stores {
		tree := mtree.TreeByName(storeName)
		require.NotNil(t, tree)

		for i := 0; i < 500; i++ {
			tree.set([]byte(string(rune('a'+i%26))+string(rune('a'+(i/26)%26))), []byte("value"))
		}
	}

	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	time.Sleep(10 * time.Millisecond)

	pool := pond.New(2, 10)
	defer pool.StopAndWait()

	snapshotDir := t.TempDir()

	err = mtree.WriteSnapshotWithContext(ctx, snapshotDir, pool)

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestMultiTreeWriteSnapshotSuccessWithContext(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

	stores := []string{store1Name, store2Name, store3Name}
	var upgrades []*TreeNameUpgrade
	for _, name := range stores {
		upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
	}
	require.NoError(t, mtree.ApplyUpgrades(upgrades))

	for _, storeName := range stores {
		tree := mtree.TreeByName(storeName)
		require.NotNil(t, tree)

		for i := 0; i < 100; i++ {
			key := []byte(storeName + string(rune('a'+i%26)))
			value := []byte("value" + string(rune('0'+i%10)))
			tree.set(key, value)
		}
	}

	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool := pond.New(4, 10)
	defer pool.StopAndWait()

	snapshotDir := t.TempDir()

	err = mtree.WriteSnapshotWithContext(ctx, snapshotDir, pool)
	require.NoError(t, err)

	// Verify all stores were written
	for _, storeName := range stores {
		storeDir := filepath.Join(snapshotDir, storeName)
		require.DirExists(t, storeDir)

		// Verify metadata file exists
		metadataFile := filepath.Join(storeDir, FileNameMetadata)
		require.FileExists(t, metadataFile)
	}

	// Verify metadata file was written at root
	metadataFile := filepath.Join(snapshotDir, MetadataFileName)
	require.FileExists(t, metadataFile)

	// Verify we can load the snapshot back
	mtree2, err := LoadMultiTree(snapshotDir, false, 0, TestAppChainID)
	require.NoError(t, err)
	defer mtree2.Close()

	require.Equal(t, mtree.Version(), mtree2.Version())
	require.Equal(t, len(mtree.trees), len(mtree2.trees))

	// Verify data integrity
	for _, storeName := range stores {
		tree1 := mtree.TreeByName(storeName)
		tree2 := mtree2.TreeByName(storeName)
		require.NotNil(t, tree1)
		require.NotNil(t, tree2)
		require.Equal(t, tree1.RootHash(), tree2.RootHash())
	}
}

func TestMultiTreeWriteSnapshotConcurrentCancellation(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

	stores := []string{store1Name, store2Name, store3Name, store4Name, store5Name, "store6", "store7", "store8"}
	var upgrades []*TreeNameUpgrade
	for _, name := range stores {
		upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
	}
	require.NoError(t, mtree.ApplyUpgrades(upgrades))

	for _, storeName := range stores {
		tree := mtree.TreeByName(storeName)
		require.NotNil(t, tree)

		for i := 0; i < 2000; i++ {
			key := []byte(storeName + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)))
			value := []byte("value" + string(rune('0'+i%10)))
			tree.set(key, value)
		}
	}

	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	pool := pond.New(2, 10)
	defer pool.StopAndWait()

	snapshotDir := t.TempDir()

	errChan := make(chan error, 1)
	go func() {
		errChan <- mtree.WriteSnapshotWithContext(ctx, snapshotDir, pool)
	}()

	time.Sleep(5 * time.Millisecond)
	cancel()

	err = <-errChan

	// Should return context.Canceled error
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	// Verify that the snapshot directory might be partially written or not at all
	// This is acceptable - the important part is that we got the error and stopped
	_, statErr := os.Stat(snapshotDir)
	if statErr == nil {
		// Directory exists, but may be incomplete - this is fine
		// The important thing is we stopped and returned an error
		t.Logf("this is acceptable")
	}
}

func TestMultiTreeWriteSnapshotEmptyTree(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

	stores := []string{"empty1", "empty2"}
	var upgrades []*TreeNameUpgrade
	for _, name := range stores {
		upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
	}
	require.NoError(t, mtree.ApplyUpgrades(upgrades))

	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool := pond.New(4, 10)
	defer pool.StopAndWait()

	snapshotDir := t.TempDir()

	err = mtree.WriteSnapshotWithContext(ctx, snapshotDir, pool)
	require.NoError(t, err)

	mtree2, err := LoadMultiTree(snapshotDir, false, 0, TestAppChainID)
	require.NoError(t, err)
	defer mtree2.Close()

	require.Equal(t, mtree.Version(), mtree2.Version())
}

func TestMultiTreeWriteSnapshotParallelWrites(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

	stores := []string{store1Name, store2Name, store3Name, store4Name, store5Name, "store6", "store7", "store8", "store9", "store10"}
	var upgrades []*TreeNameUpgrade
	for _, name := range stores {
		upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
	}
	require.NoError(t, mtree.ApplyUpgrades(upgrades))

	for _, storeName := range stores {
		tree := mtree.TreeByName(storeName)
		require.NotNil(t, tree)

		for i := 0; i < 100; i++ {
			key := []byte(storeName + string(rune('a'+i%26)))
			value := []byte("value" + string(rune('0'+i%10)))
			tree.set(key, value)
		}
	}

	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	ctx := context.Background()

	poolSizes := []int{1, 2, 4, 8}
	for _, poolSize := range poolSizes {
		t.Run("PoolSize"+string(rune('0'+poolSize)), func(t *testing.T) {
			pool := pond.New(poolSize, poolSize*10)
			defer pool.StopAndWait()

			snapshotDir := t.TempDir()

			start := time.Now()
			err = mtree.WriteSnapshotWithContext(ctx, snapshotDir, pool)
			duration := time.Since(start)

			require.NoError(t, err)
			t.Logf("Pool size %d completed in %v", poolSize, duration)

			mtree2, err := LoadMultiTree(snapshotDir, false, 0, TestAppChainID)
			require.NoError(t, err)
			defer mtree2.Close()

			require.Equal(t, mtree.Version(), mtree2.Version())
			for _, storeName := range stores {
				tree1 := mtree.TreeByName(storeName)
				tree2 := mtree2.TreeByName(storeName)
				require.Equal(t, tree1.RootHash(), tree2.RootHash())
			}
		})
	}
}

// TestMultiTreeWorkerPoolQueuedTasksShouldNotStart checks that a canceled ctx stops
// queued tree writes before they start.
func TestMultiTreeWorkerPoolQueuedTasksShouldNotStart(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

	numStores := 20
	var stores []string
	var upgrades []*TreeNameUpgrade
	for i := 0; i < numStores; i++ {
		name := "store" + string(rune('0'+i%10)) + string(rune('a'+i/10))
		stores = append(stores, name)
		upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
	}
	require.NoError(t, mtree.ApplyUpgrades(upgrades))

	// Empty trees never check ctx themselves, so only the worker group can stop them.
	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	// One worker, so the other tasks wait in the queue.
	pool := pond.New(1, numStores)
	defer pool.StopAndWait()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	snapshotDir := t.TempDir()
	err = mtree.WriteSnapshotWithContext(ctx, snapshotDir, pool)
	require.ErrorIs(t, err, context.Canceled)
	for _, name := range stores {
		require.NoDirExists(t, filepath.Join(snapshotDir, name))
	}
}

func TestLoadMultiTreeRejectsStaleMetadata(t *testing.T) {
	const wantErrMismatch = "snapshot metadata commit info does not match loaded trees"

	cases := []struct {
		name    string
		corrupt func(ci *CommitInfo)
		wantErr string
	}{
		{
			name: "hash mismatch",
			corrupt: func(ci *CommitInfo) {
				ci.StoreInfos[0].CommitId.Hash = []byte("bogus-hash-from-torn-write")
			},
			wantErr: wantErrMismatch,
		},
		{
			name: "version mismatch",
			corrupt: func(ci *CommitInfo) {
				ci.Version++
			},
			wantErr: wantErrMismatch,
		},
		{
			name: "store count mismatch",
			corrupt: func(ci *CommitInfo) {
				ci.StoreInfos = ci.StoreInfos[:len(ci.StoreInfos)-1]
			},
			wantErr: wantErrMismatch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mtree := NewEmptyMultiTree(0, 0, TestAppChainID)

			stores := []string{store1Name, store2Name}
			var upgrades []*TreeNameUpgrade
			for _, name := range stores {
				upgrades = append(upgrades, &TreeNameUpgrade{Name: name})
			}
			require.NoError(t, mtree.ApplyUpgrades(upgrades))

			for _, storeName := range stores {
				tree := mtree.TreeByName(storeName)
				require.NotNil(t, tree)
				tree.set([]byte("key"), []byte("value"))
			}

			_, err := mtree.SaveVersion(true)
			require.NoError(t, err)

			pool := pond.New(2, 10)
			defer pool.StopAndWait()

			snapshotDir := t.TempDir()
			require.NoError(t, mtree.WriteSnapshot(snapshotDir, pool))

			// Sanity check: loading the untouched snapshot succeeds.
			mtreeOK, err := LoadMultiTree(snapshotDir, false, 0, TestAppChainID)
			require.NoError(t, err)
			mtreeOK.Close()

			// Corrupt the trusted metadata to no longer match the trees on disk,
			// without touching the trees themselves or the WAL.
			staleStoreInfos := make([]StoreInfo, len(mtree.lastCommitInfo.StoreInfos))
			copy(staleStoreInfos, mtree.lastCommitInfo.StoreInfos)
			staleCommitInfo := CommitInfo{
				Version:    mtree.lastCommitInfo.Version,
				StoreInfos: staleStoreInfos,
			}
			tc.corrupt(&staleCommitInfo)
			staleMetadata := MultiTreeMetadata{
				CommitInfo:     &staleCommitInfo,
				InitialVersion: int64(mtree.initialVersion),
			}
			bz, err := staleMetadata.Marshal()
			require.NoError(t, err)
			require.NoError(t, WriteFileSync(filepath.Join(snapshotDir, MetadataFileName), bz))

			_, err = LoadMultiTree(snapshotDir, false, 0, TestAppChainID)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestRunWorkerGroup(t *testing.T) {
	labels := []string{store1Name, store2Name, store3Name}
	// Holds every task until all have started, so a failing task can't cancel a
	// sibling before it runs.
	allStarted := func() func() {
		var started sync.WaitGroup
		started.Add(len(labels))
		return func() {
			started.Done()
			started.Wait()
		}
	}

	testCases := []struct {
		name    string
		workers int
		// newTask is called once per case, so the tasks can share per-case state.
		newTask     func() func(ctx context.Context, i int) error
		expErrs     []string
		expFinished int32
	}{
		{
			name: "waits for every task",
			// one worker for three tasks, so two are still queued when Wait is entered.
			workers: 1,
			newTask: func() func(context.Context, int) error {
				return func(context.Context, int) error {
					time.Sleep(10 * time.Millisecond)
					return nil
				}
			},
			expFinished: 3,
		},
		{
			name:    "panic becomes an error without taking down running siblings",
			workers: 3,
			newTask: func() func(context.Context, int) error {
				wait := allStarted()
				return func(_ context.Context, i int) error {
					wait()
					if i == 0 {
						panic("boom")
					}
					return nil
				}
			},
			expErrs:     []string{"boom", store1Name},
			expFinished: 2,
		},
		{
			name:    "joins every error",
			workers: 3,
			newTask: func() func(context.Context, int) error {
				wait := allStarted()
				return func(_ context.Context, i int) error {
					wait()
					if i == 1 {
						return nil
					}
					return fmt.Errorf("task %d failed", i)
				}
			},
			expErrs:     []string{"task 0 failed", "task 2 failed"},
			expFinished: 3,
		},
		{
			name: "first failure skips queued tasks",
			// one worker runs the tasks in order, so the rest are still queued when task 0 fails.
			workers: 1,
			newTask: func() func(context.Context, int) error {
				return func(_ context.Context, i int) error {
					if i == 0 {
						return fmt.Errorf("task %d failed", i)
					}
					return nil
				}
			},
			expErrs:     []string{"task 0 failed", context.Canceled.Error()},
			expFinished: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pool := pond.New(tc.workers, 10)
			defer pool.StopAndWait()

			var finished atomic.Int32
			task := tc.newTask()
			err := RunWorkerGroup(context.Background(), pool, labels, func(ctx context.Context, i int) error {
				err := task(ctx, i)
				finished.Add(1)
				return err
			})

			if len(tc.expErrs) == 0 {
				require.NoError(t, err)
			}
			for _, expErr := range tc.expErrs {
				require.ErrorContains(t, err, expErr)
			}
			require.Equal(t, tc.expFinished, finished.Load())
		})
	}
}

// WriteSnapshotWithContext must not return while a tree write is still running:
// RewriteSnapshotWithContext removes the snapshot directory as soon as it
// returns an error, so an early return would race the in-flight writers.
func TestMultiTreeWriteSnapshotWaitsForInFlightWorkers(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)
	require.NoError(t, mtree.ApplyUpgrades([]*TreeNameUpgrade{{Name: store1Name}, {Name: store2Name}}))
	mtree.TreeByName(store1Name).set([]byte("k"), []byte("v"))
	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	// One worker, parked on a blocking task, so every snapshot write stays queued
	// behind it until released.
	pool := pond.New(1, 10)
	defer pool.StopAndWait()
	release := make(chan struct{})
	// Runs before StopAndWait on every exit, so a failed assertion can't hang the
	// test on the parked worker.
	releaseWorker := sync.OnceFunc(func() { close(release) })
	defer releaseWorker()
	held := make(chan struct{})
	pool.Submit(func() {
		close(held)
		<-release
	})
	<-held

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- mtree.WriteSnapshotWithContext(ctx, t.TempDir(), pool)
	}()

	select {
	case err := <-done:
		t.Fatalf("returned %v while the writes were still queued behind a running task", err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseWorker()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("WriteSnapshotWithContext did not return after the worker was released")
	}
}

func TestMultiTreeWriteSnapshotCancelsRemainingTreesOnFailure(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)
	require.NoError(t, mtree.ApplyUpgrades([]*TreeNameUpgrade{{Name: store1Name}, {Name: store2Name}}))
	mtree.TreeByName(store2Name).set([]byte("k"), []byte("v"))
	_, err := mtree.SaveVersion(true)
	require.NoError(t, err)

	// one worker runs the writes in tree order, so store2 starts only after store1 failed.
	pool := pond.New(1, 10)
	defer pool.StopAndWait()

	snapshotDir := t.TempDir()
	// a file where store1's snapshot directory belongs makes its write fail.
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, store1Name), nil, 0o600))

	err = mtree.WriteSnapshotWithContext(context.Background(), snapshotDir, pool)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "store2 must be canceled once store1 fails")
}

func TestWriteSnapshotFailsOnPanickingTree(t *testing.T) {
	mtree := NewEmptyMultiTree(0, 0, TestAppChainID)
	require.NoError(t, mtree.ApplyUpgrades([]*TreeNameUpgrade{{Name: store1Name}, {Name: store2Name}}))
	// a persisted node without a snapshot makes writeRecursive panic on a nil dereference.
	mtree.TreeByName(store1Name).root = PersistedNode{}

	pool := pond.New(1, 10)
	defer pool.StopAndWait()

	snapshotDir := t.TempDir()
	err := mtree.WriteSnapshotWithContext(context.Background(), snapshotDir, pool)
	require.ErrorContains(t, err, "panic in worker task")
	require.NoFileExists(t, filepath.Join(snapshotDir, MetadataFileName))
}
