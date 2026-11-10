package stack

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listOperations(t *testing.T, dir string) []*Operation {
	t.Helper()
	lock, err := LockOperation(dir)
	require.NoError(t, err)
	defer lock.Unlock()
	ops, err := ListOperations(dir)
	require.NoError(t, err)
	return ops
}

func startOperation(t *testing.T, dir string, claim Claim) *Operation {
	t.Helper()
	lock, err := LockOperation(dir)
	require.NoError(t, err)
	defer lock.Unlock()
	op, err := StartOperation(dir, claim)
	require.NoError(t, err)
	t.Cleanup(op.Suspend)
	return op
}

func TestOperation_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	assert.Empty(t, listOperations(t, dir), "a clone without records has no operations")

	op := startOperation(t, dir, Claim{
		Kind: "rebase", Origin: "/wt/a", PID: 1,
		Branches: []string{"b", "a", "b"}, Worktrees: []string{"/wt/a"},
	})
	assert.True(t, op.Held())
	assert.Equal(t, []string{"a", "b"}, op.Claim.Branches, "claims are stored sorted and without duplicates")

	ops := listOperations(t, dir)
	require.Len(t, ops, 1)
	assert.Same(t, op, ops[0], "a held operation is listed as itself")
	assert.True(t, ops[0].Running)

	next := op.Claim
	next.Branches = append(next.Branches, "c")
	require.NoError(t, op.SetClaim(next))
	require.NoError(t, os.WriteFile(filepath.Join(op.Dir, "gh-stack-rebase-state"), []byte("{}"), 0600))
	op.Suspend()
	assert.False(t, op.Held())
	assert.Error(t, op.SetClaim(next), "only the holder may change a claim")

	// A record with a journal is a paused operation. Its claim stands.
	ops = listOperations(t, dir)
	require.Len(t, ops, 1)
	paused := ops[0]
	assert.False(t, paused.Running)
	assert.False(t, paused.Held())
	assert.Equal(t, []string{"gh-stack-rebase-state"}, paused.Journals)
	assert.Equal(t, []string{"a", "b", "c"}, paused.Claim.Branches)
	assert.Equal(t, "/wt/a", paused.Claim.Origin)

	acquired, err := paused.Resume()
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, paused.Remove())
	assert.NoDirExists(t, paused.Dir)
	assert.Empty(t, listOperations(t, dir))
}

func TestOperation_DebrisIsCollected(t *testing.T) {
	dir := t.TempDir()
	op := startOperation(t, dir, Claim{Kind: "push", Branches: []string{"a"}})
	op.Suspend()
	assert.DirExists(t, op.Dir)
	assert.Empty(t, listOperations(t, dir), "a record without holder or journal claims nothing")
	assert.NoDirExists(t, op.Dir)
}

func TestOperation_UnreadableClaimReservesEverything(t *testing.T) {
	dir := t.TempDir()
	op := startOperation(t, dir, Claim{Kind: "rebase", Branches: []string{"a"}})
	require.NoError(t, os.WriteFile(filepath.Join(op.Dir, "gh-stack-rebase-state"), []byte("{}"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(op.Dir, claimFileName), []byte("{"), 0600))
	op.Suspend()

	// Its claim is unknown, so it reserves everything, and it can still be
	// resumed for recovery.
	ops := listOperations(t, dir)
	require.Len(t, ops, 1)
	require.ErrorContains(t, ops[0].ClaimErr, "no readable claim")
	assert.True(t, ops[0].Claims([]string{"unrelated"}, nil))
	assert.True(t, ops[0].Claims(nil, []string{"/elsewhere"}))
	assert.DirExists(t, op.Dir, "recovery state is never discarded")
	acquired, err := ops[0].Resume()
	require.NoError(t, err)
	assert.True(t, acquired)
	ops[0].Suspend()
}

func TestOperation_ResumeRefusesRunningOperation(t *testing.T) {
	dir := t.TempDir()
	op := startOperation(t, dir, Claim{Kind: "rebase", Branches: []string{"a"}})
	// A second handle stands in for another process.
	other := &Operation{Dir: op.Dir}
	acquired, err := other.Resume()
	require.NoError(t, err)
	assert.False(t, acquired)
	op.Suspend()
	acquired, err = other.Resume()
	require.NoError(t, err)
	assert.True(t, acquired)
	other.Suspend()
}

// TestOperationHolder is not a test. It is the body of the process that
// TestOperation_AcrossProcesses starts to hold an operation.
func TestOperationHolder(t *testing.T) {
	dir := os.Getenv("GH_STACK_TEST_OPERATION_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	lock, err := LockOperation(dir)
	require.NoError(t, err)
	_, err = StartOperation(dir, Claim{Kind: "rebase", Branches: []string{"a"}, Worktrees: []string{"/wt/a"}})
	require.NoError(t, err)
	lock.Unlock()
	os.Stdout.WriteString("ready\n")
	time.Sleep(time.Minute)
}

func TestOperation_AcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	holder := exec.Command(os.Args[0], "-test.run=^TestOperationHolder$")
	holder.Env = append(os.Environ(), "GH_STACK_TEST_OPERATION_DIR="+dir)
	stdout, err := holder.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, holder.Start())
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)

	ops := listOperations(t, dir)
	require.Len(t, ops, 1)
	assert.True(t, ops[0].Running, "another process holds the operation")
	assert.False(t, ops[0].Held())
	assert.Equal(t, []string{"a"}, ops[0].Claim.Branches)
	acquired, err := ops[0].Resume()
	require.NoError(t, err)
	assert.False(t, acquired)

	// A process that dies releases its operation without any cleanup.
	require.NoError(t, holder.Process.Kill())
	_ = holder.Wait()
	assert.Empty(t, listOperations(t, dir))
	assert.NoDirExists(t, ops[0].Dir)
}

func TestSaveNonBlocking_YieldsToClaims(t *testing.T) {
	for _, kind := range []string{"claimed by another operation", "paused operation", "other stack claimed", "claimed by this process"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			sf := &StackFile{Stacks: []Stack{makeStack("main", "viewed", "top"), makeStack("main", "elsewhere")}}
			require.NoError(t, Save(dir, sf))
			refresh, err := Load(dir)
			require.NoError(t, err)
			refresh.Stacks[0].Branches[0].Head = "metadata-refresh"

			claimed := []string{"top"}
			if kind == "other stack claimed" {
				claimed = []string{"elsewhere"}
			}
			op := startOperation(t, dir, Claim{Kind: "rebase", Branches: claimed})
			switch kind {
			case "claimed by another operation", "other stack claimed":
				// Forget the handle, as if another process held the record.
				heldOperations.Lock()
				delete(heldOperations.byDir, op.Dir)
				heldOperations.Unlock()
			case "paused operation":
				require.NoError(t, os.WriteFile(filepath.Join(op.Dir, "gh-stack-rebase-state"), []byte("{}"), 0600))
				op.Suspend()
			}

			SaveNonBlocking(dir, refresh)
			got, err := Load(dir)
			require.NoError(t, err)
			if kind == "other stack claimed" || kind == "claimed by this process" {
				assert.Equal(t, "metadata-refresh", got.Stacks[0].Branches[0].Head)
			} else {
				assert.Empty(t, got.Stacks[0].Branches[0].Head, "a claimed stack belongs to its operation until that operation saves")
			}
		})
	}
}

func TestJournalPath(t *testing.T) {
	state := t.TempDir()
	record := filepath.Join(state, "gh-stack-operations", "one")
	assert.Equal(t, filepath.Join(state, "journal"), JournalPath(state, "journal"))

	restore := SetJournalDir(state, record)
	assert.Equal(t, filepath.Join(record, "journal"), JournalPath(state, "journal"))
	assert.Equal(t, filepath.Join(record, "journal"), JournalPath(state+string(filepath.Separator), "journal"))
	other := t.TempDir()
	assert.Equal(t, filepath.Join(other, "journal"), JournalPath(other, "journal"), "bindings are per catalog")

	inner := SetJournalDir(state, other)
	assert.Equal(t, filepath.Join(other, "journal"), JournalPath(state, "journal"))
	inner()
	assert.Equal(t, filepath.Join(record, "journal"), JournalPath(state, "journal"))
	restore()
	assert.Equal(t, filepath.Join(state, "journal"), JournalPath(state, "journal"))
}
