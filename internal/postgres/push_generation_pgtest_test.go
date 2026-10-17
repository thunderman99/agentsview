//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// rebuildArchive stands in for a /resync: the replacement archive gets a new
// database ID and starts without this archive's PostgreSQL push state.
func (f resumeFixture) rebuildArchive(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, f.local.SetDatabaseIDForTest(t.Context(), id))
	require.NoError(t, clearPushState(t.Context(), f.local))
}

const lostStateReason = "local push state was lost, as after an archive rebuild"

// A rebuild keeps this archive's push marker but drops its watermark, so
// PostgreSQL can hold rows the rebuild dropped. The next push must be full.
func TestPushAfterArchiveRebuildIsFull(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	_, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.False(t, res.Full)

	f.rebuildArchive(t, "rebuilt")
	res, err = f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, lostStateReason, res.FullReason)
}

// A rebuild during a push mixes rows from two archives. The push must fail
// without saving its cutoff into the replacement, and the next push must be
// full.
func TestPushFailsWhenTheArchiveIsRebuiltDuringIt(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	_, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.NoError(t, f.local.InsertMessages(t.Context(), []db.Message{{
		SessionID: "new", Ordinal: 1, Role: "user", Content: "again",
		ContentLength: 5, Timestamp: "2026-03-02T00:00:00Z",
	}}))

	s := f.sync()
	s.afterPushBatch = func() { f.rebuildArchive(t, "rebuilt") }
	_, err = s.Push(t.Context(), false, nil)
	require.ErrorContains(t, err, "rebuilt while this push ran")
	saved, err := f.local.GetSyncState(t.Context(), "last_push_at")
	require.NoError(t, err)
	assert.Empty(t, saved, "the push must not save its cutoff into the replacement")

	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, lostStateReason, res.FullReason)
}

// A --full push that fails before it records its full push reports that it
// never started, so a watch keeps the --full request.
func TestFullPushFailingBeforeItsRunIsNotStarted(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, full := range []bool{false, true} {
		_, err := f.sync().PushWithOptions(ctx, storage.PushOptions{Full: full}, nil)
		require.ErrorIs(t, err, context.Canceled)
		_, notStarted := errors.AsType[storage.PushNotStartedError](err)
		assert.Equal(t, full, notStarted, "full=%t", full)
	}
}

// A failed full push saves its progress without the push's cancellation, so
// the save must still end when the archive writer never reopens, as during a
// daemon shutdown.
func TestFailedFullPushProgressSaveEnds(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	s := f.sync()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.afterPushBatch = func() {
		require.NoError(t, f.local.CloseWriter())
		cancel()
	}
	_, err := s.PushWithOptions(ctx, storage.PushOptions{Full: true}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the progress save gives up instead of waiting forever")
	require.NoError(t, f.local.ReopenWriter())
}
