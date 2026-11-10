package stack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	lockFileName          = "gh-stack.lock"
	operationLockFileName = "gh-stack-operation.lock"
	trunkLockFileName     = "gh-stack-trunk.lock"
	configLockFileName    = "gh-stack-config.lock"
)

// LockError is returned when the catalog or operation lock times out.
// Callers can check for this with errors.As to distinguish lock failures
// from other errors.
type LockError struct {
	Err error
}

func (e *LockError) Error() string { return e.Err.Error() }
func (e *LockError) Unwrap() error { return e.Err }

// StaleError is returned when the stack file was modified on disk since it
// was loaded.  This indicates another process wrote to the file concurrently.
// Callers can check for this with errors.As.
type StaleError struct {
	Err error
}

func (e *StaleError) Error() string { return e.Err.Error() }
func (e *StaleError) Unwrap() error { return e.Err }

// LockTimeout is how long Lock and LockOperation wait for an exclusive lock.
// A command also waits this long for a running operation whose claim overlaps
// its own.
var LockTimeout = 5 * time.Second

// LockRetryInterval is the sleep between non-blocking lock attempts.
const LockRetryInterval = 100 * time.Millisecond

// FileLock provides an exclusive advisory catalog or operation lock.
type FileLock struct {
	f *os.File
}

// Lock acquires an exclusive lock on the stack file in the given git directory.
// It retries with a non-blocking attempt every 100ms for up to LockTimeout.
//
// Most callers should not use Lock directly — stack.Save() acquires the lock
// automatically.  Use Lock only when you need to hold the lock across multiple
// operations (e.g. Load-Modify-SaveWithLock as an atomic unit).
func Lock(gitDir string) (*FileLock, error) {
	lock, _, err := acquireLock(filepath.Join(gitDir, lockFileName), "stack", true)
	return lock, err
}

// LockOperation guards the operation records in a repository's resolved
// common directory. Hold it only while reading or changing those records:
// commands run their work under a claim, not under this lock. Acquire it
// before the catalog lock. Save only takes the catalog lock, so it may be used
// while this is held.
func LockOperation(commonDir string) (*FileLock, error) {
	lock, _, err := acquireLock(filepath.Join(commonDir, operationLockFileName), "stack operation", true)
	return lock, err
}

// TryLockOperation attempts to acquire the operation lock without waiting.
// A false result with no error means contention; other failures are returned.
func TryLockOperation(commonDir string) (*FileLock, bool, error) {
	return acquireLock(filepath.Join(commonDir, operationLockFileName), "stack operation", false)
}

// LockTrunk excludes the fast-forward of a trunk from every checkout that
// holds LockTrunkShared. A trunk that is checked out nowhere is fast-forwarded
// by moving its ref, which is only safe while no checkout of it is under way.
//
// Lock order is trunk lock, then operation lock. Recovery of state that
// predates claims holds the operation lock throughout and may check out under
// it. That cannot deadlock: a fast-forward gives up on either lock after
// LockTimeout and leaves the local trunk alone.
func LockTrunk(commonDir string, timeout time.Duration) (*FileLock, error) {
	lock, _, err := acquireLockWithin(filepath.Join(commonDir, trunkLockFileName), "trunk", timeout, true, tryLockFile)
	return lock, err
}

// LockTrunkShared is held around the checkout of a branch that no operation
// claims. Checkouts do not exclude each other.
func LockTrunkShared(commonDir string, timeout time.Duration) (*FileLock, error) {
	lock, _, err := acquireLockWithin(filepath.Join(commonDir, trunkLockFileName), "trunk", timeout, true, tryLockFileShared)
	return lock, err
}

// LockConfig serializes the Git commands of gh-stack processes that write the
// clone's shared configuration or rename a branch. Git fails such a command
// when another one is under way, so commands that run side by side must take
// turns. It is a leaf lock, held only for that one Git command.
func LockConfig(commonDir string) (*FileLock, error) {
	lock, _, err := acquireLock(filepath.Join(commonDir, configLockFileName), "configuration", true)
	return lock, err
}

func acquireLock(path, name string, wait bool) (*FileLock, bool, error) {
	return acquireLockWithin(path, name, LockTimeout, wait, tryLockFile)
}

func acquireLockWithin(path, name string, timeout time.Duration, wait bool, try func(*os.File) error) (*FileLock, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, false, fmt.Errorf("opening lock file: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := try(f)
		if err == nil {
			return &FileLock{f: f}, true, nil
		}
		if !isLockBusy(err) {
			return nil, false, fmt.Errorf("locking %s file: %w", name, errors.Join(err, f.Close()))
		}
		if !wait {
			if err := f.Close(); err != nil {
				return nil, false, fmt.Errorf("closing lock file: %w", err)
			}
			return nil, false, nil
		}
		if time.Now().After(deadline) {
			return nil, false, &LockError{Err: errors.Join(fmt.Errorf(
				"timed out waiting for %s lock after %s — another gh-stack process may be running", name, timeout), f.Close())}
		}
		time.Sleep(LockRetryInterval)
	}
}

// Unlock releases the lock.  The lock file is intentionally left on disk to
// avoid a race where another process opens the same path, blocks on flock,
// then wakes up holding a lock on an unlinked inode while a third process
// creates a new file and locks a different inode.
func (l *FileLock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	unlockFile(l.f)
	l.f.Close()
	l.f = nil
}
