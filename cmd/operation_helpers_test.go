package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/github/gh-stack/internal/stack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// operationDirs returns the operation records left in common.
func operationDirs(t *testing.T, common string) []string {
	t.Helper()
	entries, err := os.ReadDir(stack.OperationsDir(common))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var dirs []string
	for _, entry := range entries {
		dirs = append(dirs, filepath.Join(stack.OperationsDir(common), entry.Name()))
	}
	return dirs
}

// pausedOperationDir returns the record of the only operation paused in
// common. Its journals live there.
func pausedOperationDir(t *testing.T, common string) string {
	t.Helper()
	dirs := operationDirs(t, common)
	require.Len(t, dirs, 1, "expected exactly one paused operation")
	return dirs[0]
}

// assertNoJournal asserts that no journal with the given name remains for the
// catalog in dir, beside it or in an operation record.
func assertNoJournal(t *testing.T, dir, name string) {
	t.Helper()
	assert.NoFileExists(t, filepath.Join(dir, name))
	for _, record := range operationDirs(t, dir) {
		assert.NoFileExists(t, filepath.Join(record, name))
	}
}
