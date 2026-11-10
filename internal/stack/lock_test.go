package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLock_Basic(t *testing.T) {
	dir := t.TempDir()

	lock, err := Lock(dir)
	require.NoError(t, err)
	require.NotNil(t, lock)

	lock.Unlock()
}

func TestLock_NilUnlockSafe(t *testing.T) {
	// Unlock on nil should not panic.
	var lock *FileLock
	lock.Unlock()
}

func TestLock_BlocksUntilReleased(t *testing.T) {
	dir := t.TempDir()

	lock1, err := Lock(dir)
	require.NoError(t, err)

	acquired := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		lock2, err := Lock(dir)
		if err != nil {
			errCh <- err
			return
		}
		close(acquired)
		lock2.Unlock()
	}()

	// lock2 should be blocked while lock1 is held.
	select {
	case <-acquired:
		t.Fatal("lock2 acquired while lock1 was still held")
	case err := <-errCh:
		t.Fatalf("lock2 failed: %v", err)
	case <-time.After(300 * time.Millisecond):
		// expected — lock2 is waiting
	}

	lock1.Unlock()

	// After releasing lock1, lock2 should acquire promptly.
	select {
	case <-acquired:
		// success
	case err := <-errCh:
		t.Fatalf("lock2 failed after lock1 released: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("lock2 did not acquire after lock1 was released")
	}
}

func TestLock_SerializesConcurrentAccess(t *testing.T) {
	dir := t.TempDir()

	// Write an initial stack file with 0 stacks.
	sf := &StackFile{SchemaVersion: 1, Stacks: []Stack{}}
	require.NoError(t, Save(dir, sf))

	// Run 10 concurrent goroutines, each adding a stack under lock.
	// Uses Lock + Load + writeStackFile for atomic read-modify-write.
	errCh := make(chan error, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			lock, err := Lock(dir)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d Lock: %w", idx, err)
				return
			}
			defer lock.Unlock()

			loaded, err := Load(dir)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d Load: %w", idx, err)
				return
			}

			loaded.AddStack(makeStack("main", "branch"))
			if err := writeStackFile(dir, loaded); err != nil {
				errCh <- fmt.Errorf("goroutine %d writeStackFile: %w", idx, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}

	// All 10 stacks should be present — no lost updates.
	final, err := Load(dir)
	require.NoError(t, err)
	assert.Len(t, final.Stacks, 10, "all concurrent writes should be preserved")
}

func TestLock_FileLeftOnDisk(t *testing.T) {
	dir := t.TempDir()

	lock, err := Lock(dir)
	require.NoError(t, err)
	lock.Unlock()

	// Lock file should still exist after unlock (no os.Remove race).
	_, err = os.Stat(filepath.Join(dir, lockFileName))
	require.NoError(t, err, "lock file should remain on disk after unlock")

	lock2, err := Lock(dir)
	require.NoError(t, err, "should be able to re-lock after unlock")
	lock2.Unlock()
}

func TestLock_TimesOut(t *testing.T) {
	dir := t.TempDir()

	// Hold the lock so the second attempt can never acquire it.
	lock1, err := Lock(dir)
	require.NoError(t, err)
	defer lock1.Unlock()

	// Save original timeout and set a short one for the test.
	origTimeout := LockTimeout
	LockTimeout = 200 * time.Millisecond
	defer func() { LockTimeout = origTimeout }()

	start := time.Now()
	lock2, err := Lock(dir)
	elapsed := time.Since(start)

	assert.Nil(t, lock2, "should not acquire lock")
	require.Error(t, err)

	var lockErr *LockError
	require.True(t, errors.As(err, &lockErr), "error should be *LockError, got %T", err)
	assert.Contains(t, lockErr.Error(), "timed out")

	// Should have waited roughly LockTimeout before giving up.
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond, "should wait near the timeout")
}

func TestSave_MergesChangesToOtherStacks(t *testing.T) {
	dir := t.TempDir()
	sf := &StackFile{SchemaVersion: 1, Stacks: []Stack{makeStack("main", "mine"), makeStack("main", "theirs")}}
	require.NoError(t, Save(dir, sf))

	loaded, err := Load(dir)
	require.NoError(t, err)

	// Another process changes its own stack, adds one, and removes none of ours.
	other, err := Load(dir)
	require.NoError(t, err)
	other.Stacks[1].Branches[0].Head = "theirs-head"
	other.AddStack(makeStack("main", "sneaky"))
	require.NoError(t, Save(dir, other))

	loaded.AddStack(makeStack("main", "my-branch"))
	mine := &loaded.Stacks[0]
	mine.Branches[0].Head = "mine-head"
	require.NoError(t, Save(dir, loaded))

	final, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, final.Stacks, 4)
	assert.Equal(t, "mine-head", final.Stacks[0].Branches[0].Head)
	assert.Equal(t, "theirs-head", final.Stacks[1].Branches[0].Head, "an untouched stack must keep the other writer's change")
	assert.Equal(t, []string{"sneaky"}, final.Stacks[2].BranchNames())
	assert.Equal(t, []string{"my-branch"}, final.Stacks[3].BranchNames())

	// The view keeps its own stacks, so earlier pointers stay valid and a
	// later save still publishes only this view's changes.
	assert.Same(t, mine, &loaded.Stacks[0])
	mine.Branches[0].Head = "mine-again"
	other.Stacks[1].Branches[0].Head = "theirs-again"
	require.NoError(t, Save(dir, other))
	require.NoError(t, Save(dir, loaded))
	final, err = Load(dir)
	require.NoError(t, err)
	require.Len(t, final.Stacks, 4)
	assert.Equal(t, "mine-again", final.Stacks[0].Branches[0].Head)
	assert.Equal(t, "theirs-again", final.Stacks[1].Branches[0].Head)
}

func TestSave_DetectsStaleStack(t *testing.T) {
	for _, change := range []string{"changed", "removed"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			sf := &StackFile{SchemaVersion: 1, Stacks: []Stack{makeStack("main", "shared"), makeStack("main", "bystander")}}
			require.NoError(t, Save(dir, sf))

			loaded, err := Load(dir)
			require.NoError(t, err)
			other, err := Load(dir)
			require.NoError(t, err)
			if change == "changed" {
				other.Stacks[0].Branches[0].Head = "sneaky"
			} else {
				other.RemoveStack(0)
			}
			require.NoError(t, Save(dir, other))
			before, err := os.ReadFile(stackFilePath(dir))
			require.NoError(t, err)

			loaded.Stacks[0].Branches[0].Head = "mine"
			err = Save(dir, loaded)
			var staleErr *StaleError
			require.True(t, errors.As(err, &staleErr), "error should be *StaleError, got %T", err)
			assert.Contains(t, staleErr.Error(), "modified by another process")
			after, err := os.ReadFile(stackFilePath(dir))
			require.NoError(t, err)
			assert.Equal(t, before, after, "a refused save must not write")
		})
	}
}

func TestSave_RefusesBranchInTwoStacks(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, &StackFile{SchemaVersion: 1, Stacks: []Stack{}}))
	loaded, err := Load(dir)
	require.NoError(t, err)
	other, err := Load(dir)
	require.NoError(t, err)
	other.AddStack(makeStack("main", "feature"))
	require.NoError(t, Save(dir, other))

	loaded.AddStack(makeStack("main", "feature", "more"))
	err = Save(dir, loaded)
	var staleErr *StaleError
	require.ErrorAs(t, err, &staleErr)
	assert.Contains(t, err.Error(), `branch "feature"`)

	// A trunk may be shared, and may be a branch of another stack.
	loaded.Stacks[0] = makeStack("feature", "on-top")
	require.NoError(t, Save(dir, loaded))
	final, err := Load(dir)
	require.NoError(t, err)
	assert.Len(t, final.Stacks, 2)
}

func TestSave_ReplacesChangedStackInPlace(t *testing.T) {
	dir := t.TempDir()
	sf := &StackFile{SchemaVersion: 1, Stacks: []Stack{makeStack("main", "a1", "a2"), makeStack("main", "b1")}}
	require.NoError(t, Save(dir, sf))
	loaded, err := Load(dir)
	require.NoError(t, err)
	other, err := Load(dir)
	require.NoError(t, err)
	other.AddStack(makeStack("main", "c1"))
	require.NoError(t, Save(dir, other))

	// Rename one branch, drop another, and remove the second stack.
	loaded.Stacks[0] = makeStack("main", "a1", "a3")
	loaded.RemoveStack(1)
	require.NoError(t, Save(dir, loaded))

	final, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, final.Stacks, 2)
	assert.Equal(t, []string{"a1", "a3"}, final.Stacks[0].BranchNames())
	assert.Equal(t, []string{"c1"}, final.Stacks[1].BranchNames())
}

func TestSave_ConcurrentWritersOfDifferentStacks(t *testing.T) {
	dir := t.TempDir()
	const writers, rounds = 8, 25
	initial := &StackFile{SchemaVersion: 1}
	for i := range writers {
		initial.AddStack(makeStack("main", fmt.Sprintf("branch-%d", i)))
	}
	require.NoError(t, Save(dir, initial))

	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			sf, err := Load(dir)
			if err != nil {
				errs <- err
				return
			}
			for round := range rounds {
				sf.Stacks[i].Branches[0].Head = fmt.Sprintf("head-%d-%d", i, round)
				if err := Save(dir, sf); err != nil {
					errs <- fmt.Errorf("writer %d round %d: %w", i, round, err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	final, err := Load(dir)
	require.NoError(t, err)
	require.Len(t, final.Stacks, writers)
	for i := range writers {
		assert.Equal(t, fmt.Sprintf("head-%d-%d", i, rounds-1), final.Stacks[i].Branches[0].Head)
	}
}

func TestSave_AllowsWriteWhenFileUnchanged(t *testing.T) {
	dir := t.TempDir()

	// Write, load, modify, save — no concurrent changes.
	sf := &StackFile{SchemaVersion: 1, Stacks: []Stack{}}
	require.NoError(t, Save(dir, sf))

	loaded, err := Load(dir)
	require.NoError(t, err)

	loaded.AddStack(makeStack("main", "feature"))
	require.NoError(t, Save(dir, loaded))

	// Verify the write actually persisted.
	final, err := Load(dir)
	require.NoError(t, err)
	assert.Len(t, final.Stacks, 1)
}

func TestSave_AllowsFirstWrite(t *testing.T) {
	dir := t.TempDir()

	// File doesn't exist — Load returns nil checksum, Save should succeed.
	sf, err := Load(dir)
	require.NoError(t, err)
	assert.Empty(t, sf.Stacks)

	sf.AddStack(makeStack("main", "first"))
	require.NoError(t, Save(dir, sf), "first save to a new file should succeed")

	final, err := Load(dir)
	require.NoError(t, err)
	assert.Len(t, final.Stacks, 1)
}

func TestSave_DoubleSaveSucceeds(t *testing.T) {
	dir := t.TempDir()

	sf, err := Load(dir)
	require.NoError(t, err)

	sf.AddStack(makeStack("main", "first"))
	require.NoError(t, Save(dir, sf), "first save should succeed")

	// A second Save on the same instance must not spuriously fail —
	// writeStackFile refreshes loadChecksum after writing.
	sf.AddStack(makeStack("main", "second"))
	require.NoError(t, Save(dir, sf), "second save on same instance should succeed")

	final, err := Load(dir)
	require.NoError(t, err)
	assert.Len(t, final.Stacks, 2)
}

func TestLockOperation_IndependentCatalogLock(t *testing.T) {
	dir := t.TempDir()
	operation, err := LockOperation(dir)
	require.NoError(t, err)
	defer operation.Unlock()

	sf, err := Load(dir)
	require.NoError(t, err)
	sf.AddStack(makeStack("main", "feature"))
	require.NoError(t, Save(dir, sf), "Save must not retake the operation lock")

	catalog, err := Lock(dir)
	require.NoError(t, err)
	defer catalog.Unlock()
	sf.AddStack(makeStack("main", "other"))
	require.NoError(t, SaveWithLock(dir, sf, catalog))

	start := time.Now()
	contender, acquired, err := TryLockOperation(dir)
	require.NoError(t, err)
	assert.False(t, acquired)
	assert.Nil(t, contender)
	assert.Less(t, time.Since(start), time.Second)

	operation.Unlock()
	operation.Unlock()
	contender, acquired, err = TryLockOperation(dir)
	require.NoError(t, err)
	require.True(t, acquired, "catalog lock must not prevent an operation lock")
	contender.Unlock()

	assert.FileExists(t, filepath.Join(dir, operationLockFileName))
	assert.FileExists(t, filepath.Join(dir, lockFileName))
}

func TestTryLockOperation_Errors(t *testing.T) {
	for _, kind := range []string{"missing directory", "directory at lock path"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if kind == "missing directory" {
				dir = filepath.Join(dir, "missing")
			} else {
				require.NoError(t, os.Mkdir(filepath.Join(dir, operationLockFileName), 0755))
			}
			lock, acquired, err := TryLockOperation(dir)
			require.Error(t, err)
			assert.Nil(t, lock)
			assert.False(t, acquired)
			var lockErr *LockError
			assert.False(t, errors.As(err, &lockErr), "real I/O failure is not contention")
		})
	}

	t.Run("invalid descriptor is not contention", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "lock")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		err = tryLockFile(f)
		require.Error(t, err)
		assert.False(t, isLockBusy(err))
	})
}

func TestLockOperation_TimesOut(t *testing.T) {
	dir := t.TempDir()
	lock, err := LockOperation(dir)
	require.NoError(t, err)
	defer lock.Unlock()
	originalTimeout := LockTimeout
	LockTimeout = 100 * time.Millisecond
	defer func() { LockTimeout = originalTimeout }()

	other, err := LockOperation(dir)
	require.Error(t, err)
	assert.Nil(t, other)
	var lockErr *LockError
	require.ErrorAs(t, err, &lockErr)
	assert.Contains(t, err.Error(), "stack operation lock")
}

func TestLockOperation_SerializesMutationSnapshots(t *testing.T) {
	dir := t.TempDir()
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			lock, err := LockOperation(dir)
			if err != nil {
				errs <- err
				return
			}
			defer lock.Unlock()
			sf, err := Load(dir)
			if err != nil {
				errs <- err
				return
			}
			sf.AddStack(makeStack("main", fmt.Sprintf("branch-%d", i)))
			errs <- Save(dir, sf)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	sf, err := Load(dir)
	require.NoError(t, err)
	assert.Len(t, sf.Stacks, 4)
}

func TestSaveNonBlocking_OperationAndCatalogGuards(t *testing.T) {
	for _, kind := range []string{"uncontended", "operation lock", "catalog lock", "stale", "pending migration"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			sf := &StackFile{Stacks: []Stack{makeStack("main", "original")}}
			require.NoError(t, Save(dir, sf))
			refresh, err := Load(dir)
			require.NoError(t, err)
			refresh.Stacks[0].Branches[0].Head = "metadata-refresh"
			checksum := append([]byte(nil), refresh.loadChecksum...)

			var held *FileLock
			switch kind {
			case "operation lock":
				held, err = LockOperation(dir)
			case "catalog lock":
				held, err = Lock(dir)
			case "stale":
				sf.Stacks[0].Branches[0].Head = "critical-write"
				err = Save(dir, sf)
			case "pending migration":
				err = os.WriteFile(filepath.Join(dir, migrationFileName), []byte("{}"), 0600)
			}
			require.NoError(t, err)
			defer held.Unlock()

			start := time.Now()
			SaveNonBlocking(dir, refresh)
			if held != nil {
				assert.Less(t, time.Since(start), time.Second, "a held lock must not delay an optional refresh")
			}
			held.Unlock()
			got, err := Load(dir)
			require.NoError(t, err)
			if kind == "uncontended" {
				assert.Equal(t, "metadata-refresh", got.Stacks[0].Branches[0].Head)
				assert.NotEqual(t, checksum, refresh.loadChecksum)
			} else {
				assert.NotEqual(t, "metadata-refresh", got.Stacks[0].Branches[0].Head)
				assert.Equal(t, checksum, refresh.loadChecksum)
			}
			lock, acquired, err := TryLockOperation(dir)
			require.NoError(t, err)
			require.True(t, acquired, "a skipped refresh must release the operation lock")
			lock.Unlock()
		})
	}
}

func TestSave_AtomicReaderVisibility(t *testing.T) {
	dir := t.TempDir()
	sf := &StackFile{Stacks: []Stack{makeStack("main", "feature")}}
	require.NoError(t, Save(dir, sf))
	stop := make(chan struct{})
	errs := make(chan error, 2)
	var reads atomic.Int64
	var readers sync.WaitGroup
	for range 2 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, err := Load(dir)
				if err != nil {
					errs <- err
					return
				}
				if len(got.Stacks) != 1 || len(got.Stacks[0].Branches) != 1 ||
					got.Stacks[0].Trunk.Head != got.Stacks[0].Branches[0].Base {
					errs <- fmt.Errorf("reader observed an incomplete catalog: %#v", got)
					return
				}
				reads.Add(1)
			}
		})
	}
	var writeErr error
	for i := range 50 {
		head := strings.Repeat(fmt.Sprintf("%04d", i), 1024)
		sf.Stacks[0].Trunk.Head = head
		sf.Stacks[0].Branches[0].Base = head
		if writeErr = Save(dir, sf); writeErr != nil {
			break
		}
	}
	close(stop)
	readers.Wait()
	close(errs)
	require.NoError(t, writeErr)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Positive(t, reads.Load())
	temps, err := filepath.Glob(filepath.Join(dir, "."+stackFileName+"-*"))
	require.NoError(t, err)
	assert.Empty(t, temps)
}

func TestAtomicPublication_PreservesExistingFiles(t *testing.T) {
	t.Run("no overwrite on exclusive publication", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "backup")
		require.NoError(t, writeFileAtomic(path, []byte("original"), 0600, false))
		err := writeFileAtomic(path, []byte("replacement"), 0600, false)
		require.ErrorIs(t, err, os.ErrExist)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "original", string(data))
		temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".backup-*"))
		require.NoError(t, err)
		assert.Empty(t, temps)
	})

	t.Run("failed replace keeps destination", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "destination")
		require.NoError(t, os.WriteFile(path, []byte("original"), 0600))
		require.Error(t, publishFile(filepath.Join(dir, "missing"), path, true))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "original", string(data))
	})

	t.Run("non-regular destination is not replaced", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "destination")
		require.NoError(t, os.Mkdir(path, 0755))
		require.Error(t, writeFileAtomic(path, []byte("replacement"), 0600, true))
		assert.DirExists(t, path)
	})

	t.Run("mode survives replacement", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix permission bits")
		}
		dir := t.TempDir()
		sf := &StackFile{Stacks: []Stack{makeStack("main", "feature")}}
		require.NoError(t, Save(dir, sf))
		require.NoError(t, os.Chmod(stackFilePath(dir), 0640))
		sf.AddStack(makeStack("main", "other"))
		require.NoError(t, Save(dir, sf))
		info, err := os.Stat(stackFilePath(dir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0640), info.Mode().Perm())
	})
}

func TestReadStateFile(t *testing.T) {
	t.Run("reads complete bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state")
		for _, want := range [][]byte{
			{},
			{0x00, 0xff, 0x0a},
			[]byte(strings.Repeat("state\x00\xff\n", 16384)),
		} {
			require.NoError(t, os.WriteFile(path, want, 0600))
			got, err := ReadStateFile(path)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing")
		_, err := ReadStateFile(path)
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.NoFileExists(t, path)
	})

	t.Run("directory", func(t *testing.T) {
		path := t.TempDir()
		_, err := ReadStateFile(path)
		var pathErr *os.PathError
		require.ErrorAs(t, err, &pathErr)
		assert.Equal(t, path, pathErr.Path)
		assert.DirExists(t, path)
	})

	t.Run("unreadable file", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("requires Unix permission enforcement")
		}
		path := filepath.Join(t.TempDir(), "state")
		require.NoError(t, os.WriteFile(path, []byte("private state"), 0600))
		require.NoError(t, os.Chmod(path, 0))
		t.Cleanup(func() { assert.NoError(t, os.Chmod(path, 0600)) })
		_, err := ReadStateFile(path)
		require.ErrorIs(t, err, os.ErrPermission)
	})
}

func TestWriteAtomic(t *testing.T) {
	t.Run("creates and replaces exact bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "gh-stack-rebase-state")
		for _, data := range [][]byte{
			[]byte(`{"phase":"rebasing","branch":"feature"}`),
			[]byte(`{"phase":"done"}`),
			{0x00, 0xff, 0x0a},
			{},
		} {
			require.NoError(t, WriteAtomic(path, data))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, data, got)
		}
		temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".gh-stack-rebase-state-*"))
		require.NoError(t, err)
		assert.Empty(t, temps)
	})

	t.Run("missing parent is reported", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "missing")
		require.ErrorIs(t, WriteAtomic(filepath.Join(parent, "state"), []byte("{}")), os.ErrNotExist)
		assert.NoDirExists(t, parent)
	})

	t.Run("non-regular target is preserved", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state")
		require.NoError(t, os.Mkdir(path, 0755))
		require.Error(t, WriteAtomic(path, []byte("{}")))
		assert.DirExists(t, path)
	})
}

func TestSave_FailedPublicationPreservesChecksum(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory permission enforcement")
	}
	dir := t.TempDir()
	sf := &StackFile{Stacks: []Stack{makeStack("main", "original")}}
	require.NoError(t, Save(dir, sf))
	checksum := append([]byte(nil), sf.loadChecksum...)
	before, err := os.ReadFile(stackFilePath(dir))
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0500))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, info.Mode().Perm())) })

	sf.AddStack(makeStack("main", "unsaved"))
	require.Error(t, Save(dir, sf))
	assert.Equal(t, checksum, sf.loadChecksum)
	after, err := os.ReadFile(stackFilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, os.Chmod(dir, info.Mode().Perm()))
	require.NoError(t, Save(dir, sf), "a failed publication must remain retryable")
}

func TestMigrateLegacyState_TakesCatalogLock(t *testing.T) {
	dir := t.TempDir()
	catalogs := []migrationCatalog{
		writeMigrationTestCatalog(t, dir, "worktrees/linked/gh-stack", migrationTestFile(makeStack("main", "linked"))),
	}
	operation, err := LockOperation(dir)
	require.NoError(t, err)
	defer operation.Unlock()
	catalog, err := Lock(dir)
	require.NoError(t, err)
	defer catalog.Unlock()
	originalTimeout := LockTimeout
	LockTimeout = 0
	defer func() { LockTimeout = originalTimeout }()

	var lockErr *LockError
	require.ErrorAs(t, MigrateLegacyState(dir), &lockErr)
	assertMigrationOriginals(t, dir, catalogs, false)
	catalog.Unlock()
	require.NoError(t, MigrateLegacyState(dir))
	assertMigrationOriginals(t, dir, catalogs, true)
}

// Writers that own different stacks keep long-lived views and save in a
// random order. After every save the catalog must hold exactly what each
// writer last intended for its own stacks.
func TestSave_RandomizedWritersOfDifferentStacks(t *testing.T) {
	const writers, steps = 4, 60
	for seed := range int64(15) {
		rng := rand.New(rand.NewSource(seed))
		dir := t.TempDir()
		created := 0
		newStack := func(writer int) Stack {
			created++
			name := fmt.Sprintf("w%d-s%d", writer, created)
			return Stack{ID: name, Trunk: BranchRef{Branch: "main"}, Branches: []BranchRef{{Branch: name + "-1"}, {Branch: name + "-2"}}}
		}
		// intended[w] maps a stack ID to its JSON as writer w last left it.
		intended := make([]map[string]string, writers)
		encode := func(s *Stack) string {
			data, err := json.Marshal(s)
			require.NoError(t, err)
			return string(data)
		}
		initial := &StackFile{SchemaVersion: 1}
		for w := range writers {
			s := newStack(w)
			intended[w] = map[string]string{s.ID: encode(&s)}
			initial.AddStack(s)
		}
		require.NoError(t, Save(dir, initial))
		views := make([]*StackFile, writers)
		for w := range writers {
			view, err := Load(dir)
			require.NoError(t, err)
			views[w] = view
		}

		for step := range steps {
			w := rng.Intn(writers)
			view := views[w]
			var owned []int
			for i := range view.Stacks {
				if _, ok := intended[w][view.Stacks[i].ID]; ok {
					owned = append(owned, i)
				}
			}
			action := rng.Intn(7)
			if len(owned) == 0 {
				action = 0
			}
			var target *Stack
			if len(owned) > 0 {
				target = &view.Stacks[owned[rng.Intn(len(owned))]]
			}
			switch action {
			case 0:
				s := newStack(w)
				view.AddStack(s)
				intended[w][s.ID] = encode(&s)
			case 1:
				delete(intended[w], target.ID)
				view.RemoveStack(view.IndexOfStack(target))
			case 2:
				target.Branches[len(target.Branches)-1].Branch = fmt.Sprintf("%s-renamed-%d", target.ID, step)
				intended[w][target.ID] = encode(target)
			case 3:
				for i := range target.Branches {
					target.Branches[i].Branch = fmt.Sprintf("%s-replaced-%d-%d", target.ID, step, i)
				}
				intended[w][target.ID] = encode(target)
			case 4:
				fresh, err := Load(dir)
				require.NoError(t, err)
				views[w] = fresh
				continue
			default:
				target.Branches[0].Head = fmt.Sprintf("head-%d", step)
				intended[w][target.ID] = encode(target)
			}
			require.NoError(t, Save(dir, view), "seed %d step %d writer %d action %d", seed, step, w, action)

			final, err := Load(dir)
			require.NoError(t, err)
			var got, want []string
			for i := range final.Stacks {
				got = append(got, encode(&final.Stacks[i]))
			}
			for w := range writers {
				for _, s := range intended[w] {
					want = append(want, s)
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			require.Equal(t, want, got, "seed %d step %d writer %d action %d", seed, step, w, action)
		}
	}
}
