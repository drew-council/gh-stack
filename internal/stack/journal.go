package stack

import (
	"path/filepath"
	"sync"
)

// Operations on different stacks can pause at the same time, so their
// recovery journals cannot share one path beside the catalog. A command binds
// its catalog directory to its own operation directory for its lifetime, and
// journal readers and writers resolve their file through that binding.
var journalDirs = struct {
	sync.Mutex
	byState map[string]string
}{byState: make(map[string]string)}

// SetJournalDir routes the journals of the catalog in stateDir to journalDir
// until the returned function is called.
func SetJournalDir(stateDir, journalDir string) (restore func()) {
	key := filepath.Clean(stateDir)
	journalDirs.Lock()
	previous, had := journalDirs.byState[key]
	journalDirs.byState[key] = journalDir
	journalDirs.Unlock()
	return func() {
		journalDirs.Lock()
		defer journalDirs.Unlock()
		if had {
			journalDirs.byState[key] = previous
		} else {
			delete(journalDirs.byState, key)
		}
	}
}

// JournalPath returns the path of the named journal for the catalog in
// stateDir. Without a binding the journal sits beside the catalog, which is
// also where releases before per-operation journals wrote it.
func JournalPath(stateDir, name string) string {
	journalDirs.Lock()
	dir, ok := journalDirs.byState[filepath.Clean(stateDir)]
	journalDirs.Unlock()
	if !ok {
		dir = stateDir
	}
	return filepath.Join(dir, name)
}
