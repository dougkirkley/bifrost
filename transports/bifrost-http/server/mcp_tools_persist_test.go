package server

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingToolsConfigStore captures every UpdateMCPClientSnapshot call for assertion. failErr is
// returned for the first failTimes calls (every call when failTimes is 0); hold, when set, blocks the
// first call until it is closed, after signalling started.
type recordingToolsConfigStore struct {
	configstore.ConfigStore
	mu        sync.Mutex
	calls     []toolsUpdateCall
	attempts  int
	failErr   error
	failTimes int
	hold      chan struct{}
	started   chan struct{}
}

type toolsUpdateCall struct {
	clientID     string
	tools        map[string]schemas.ChatTool
	mapping      map[string]string
	instructions string
	prompts      []mcp.Prompt
}

func (s *recordingToolsConfigStore) UpdateMCPClientSnapshot(ctx context.Context, clientID string, snapshot *schemas.MCPServerSnapshot) error {
	s.mu.Lock()
	s.attempts++
	first := s.attempts == 1
	s.mu.Unlock()
	if first && s.hold != nil {
		close(s.started)
		<-s.hold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil && (s.failTimes == 0 || s.attempts <= s.failTimes) {
		return s.failErr
	}
	s.calls = append(s.calls, toolsUpdateCall{clientID: clientID, tools: snapshot.Tools, mapping: snapshot.ToolNameMapping, instructions: snapshot.Instructions, prompts: snapshot.Prompts})
	return nil
}

func (s *recordingToolsConfigStore) snapshotCalls() []toolsUpdateCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func (s *recordingToolsConfigStore) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// fastSnapshotRetries makes a failed snapshot write retry within milliseconds for the test.
func fastSnapshotRetries(t *testing.T) {
	t.Helper()
	prev := mcpSnapshotRetryBackoff
	mcpSnapshotRetryBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { mcpSnapshotRetryBackoff = prev })
}

// TestPersistMCPClientSnapshot_WritesThroughConfigStore pins the OSS side of the
// funnel: the tools-change callback registered on the MCP manager must
// persist the fresh result via ConfigStore.UpdateMCPClientSnapshot — the
// targeted column update, not a full-row overwrite.
func TestPersistMCPClientSnapshot_WritesThroughConfigStore(t *testing.T) {
	store := &recordingToolsConfigStore{}
	s := &BifrostHTTPServer{Config: &lib.Config{ConfigStore: store}}

	tools := map[string]schemas.ChatTool{"echo": {Type: "function"}}
	mapping := map[string]string{"echo": "echo-server"}
	prompts := []mcp.Prompt{mcp.NewPrompt("client-review")}
	s.PersistMCPClientSnapshot(context.Background(), "client-1", &schemas.MCPServerSnapshot{Tools: tools, ToolNameMapping: mapping, Instructions: "Use me carefully.", Prompts: prompts})

	require.Eventually(t, func() bool { return len(store.snapshotCalls()) == 1 }, 2*time.Second, time.Millisecond)
	calls := store.snapshotCalls()
	assert.Equal(t, "client-1", calls[0].clientID)
	assert.Equal(t, tools, calls[0].tools)
	assert.Equal(t, mapping, calls[0].mapping)
	// The instructions must ride the same write: a per-call client holds no connection to
	// re-read them from, so if they are dropped here they are lost across a restart.
	assert.Equal(t, "Use me carefully.", calls[0].instructions)
	assert.Equal(t, prompts, calls[0].prompts, "prompts ride the same write")
}

// TestPersistMCPClientSnapshot_NilConfigStore_NoOp confirms the nil-guard
// convention this package uses elsewhere (Config or ConfigStore not yet
// wired) — must not panic.
func TestPersistMCPClientSnapshot_NilConfigStore_NoOp(t *testing.T) {
	s := &BifrostHTTPServer{Config: &lib.Config{ConfigStore: nil}}
	assert.NotPanics(t, func() {
		s.PersistMCPClientSnapshot(context.Background(), "client-1", &schemas.MCPServerSnapshot{Tools: map[string]schemas.ChatTool{}, ToolNameMapping: map[string]string{}})
	})
}

// TestPersistMCPClientSnapshot_RetriesUntilWritten: the manager records a snapshot's hash before it
// is saved, so an identical later discovery never asks again. A failed write is retried until it lands.
func TestPersistMCPClientSnapshot_RetriesUntilWritten(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()
	fastSnapshotRetries(t)

	store := &recordingToolsConfigStore{failErr: fmt.Errorf("db unavailable"), failTimes: 2}
	s := &BifrostHTTPServer{Config: &lib.Config{ConfigStore: store}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.PersistMCPClientSnapshot(ctx, "client-1", &schemas.MCPServerSnapshot{Instructions: "v1"})
	require.Eventually(t, func() bool { return len(store.snapshotCalls()) == 1 }, 2*time.Second, time.Millisecond, "the failed write was not retried")
	assert.Equal(t, 3, store.attemptCount())
	assert.Equal(t, "v1", store.snapshotCalls()[0].instructions)
}

// TestPersistMCPClientSnapshot_NewestSnapshotWins: changes that arrive while a write is in flight
// are written after it, and only the newest, so an older snapshot never lands last.
func TestPersistMCPClientSnapshot_NewestSnapshotWins(t *testing.T) {
	store := &recordingToolsConfigStore{hold: make(chan struct{}), started: make(chan struct{})}
	s := &BifrostHTTPServer{Config: &lib.Config{ConfigStore: store}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.PersistMCPClientSnapshot(ctx, "client-1", &schemas.MCPServerSnapshot{Instructions: "v1"})
	<-store.started
	var wg sync.WaitGroup
	for _, version := range []string{"v2", "v3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.PersistMCPClientSnapshot(ctx, "client-1", &schemas.MCPServerSnapshot{Instructions: version})
		}()
		time.Sleep(20 * time.Millisecond) // v3 is queued after v2
	}
	close(store.hold)
	wg.Wait()

	require.Eventually(t, func() bool {
		calls := store.snapshotCalls()
		return len(calls) > 0 && calls[len(calls)-1].instructions == "v3"
	}, 2*time.Second, time.Millisecond, "the newest snapshot did not land last: %+v", store.snapshotCalls())
	time.Sleep(50 * time.Millisecond)
	calls := store.snapshotCalls()
	assert.Equal(t, "v3", calls[len(calls)-1].instructions, "an older snapshot landed after the newest: %+v", calls)
	assert.Equal(t, []string{"v1", "v3"}, []string{calls[0].instructions, calls[len(calls)-1].instructions})
	assert.Len(t, calls, 2, "v2 was superseded before it was written")
}

// TestPersistMCPClientSnapshot_DeletedClientIsNotRetried: a client removed before its snapshot was
// saved has no row to write, so the write is dropped instead of retried.
func TestPersistMCPClientSnapshot_DeletedClientIsNotRetried(t *testing.T) {
	prevLogger := logger
	logger = noopTestLogger{}
	defer func() { logger = prevLogger }()
	fastSnapshotRetries(t)

	store := &recordingToolsConfigStore{failErr: configstore.ErrNotFound}
	s := &BifrostHTTPServer{Config: &lib.Config{ConfigStore: store}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.PersistMCPClientSnapshot(ctx, "client-1", &schemas.MCPServerSnapshot{})
	require.Eventually(t, func() bool { return store.attemptCount() == 1 }, 2*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, store.attemptCount())
}

// TestSyncMCPServersAfterToolsChange_NilHandler_NoOp confirms the nil-guard
// convention this package uses elsewhere — MCPServerHandler not yet wired
// (e.g. in a partially-constructed test server) must not panic. The
// underlying SyncMCPServer call itself is exercised the same way the
// pre-existing SetClientSnapshot wrapper's identical call already is: only via
// full integration, since MCPServerHandler has no fake-able seam from this
// package.
func TestSyncMCPServersAfterToolsChange_NilHandler_NoOp(t *testing.T) {
	s := &BifrostHTTPServer{}
	assert.NotPanics(t, func() {
		s.SyncMCPServersAfterToolsChange(context.Background(), "client-1")
	})
}
