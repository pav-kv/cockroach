// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/logstore"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// Raft log storage writes are carried out under raftMu which blocks all other
// writes for this raft instance. Note that it does not prevent concurrent log
// reads from within RawNode when it only holds Replica.mu. However, RawNode
// never attempts to read log indices that have pending writes. See also the
// replicaRaftStorage comment for more details.
//
// TODO(#131063): there is one subtle exception - the log can be compacted
// concurrently with reading from its "readable" prefix. Incorrect ordering of
// raftMu/mu updates during log compactions can cause read attempts for deleted
// indices. We should fix it.
//
// For performance reasons, the raft log storage state (such as the last index)
// needs to be accessible under Replica.mu. When appending is done, the state
// needs to be transferred into the Replica.mu section.
//
// Appends can truncate a suffix of the log and overwrite it with entries
// proposed by a new leader. An important corollary is that the log state
// observed by Replica.mu-only sections is stale, because there can be a
// concurrent raftMu writer changing these indices/entries.
//
// TODO(pav-kv): audit if there are any places relying on this stale raft log
// state too heavily. The consistent source of truth about the raft log state
// under Replica.mu is the RawNode / unstable, and preferably should be used.
//
// Raft log storage writes are asynchronous, in a limited sense. The write
// batches are committed to Pebble immediately, but flush/sync is communicated
// asynchronously (with a few synchronous case exceptions, see the code down the
// appendRaftMuLocked stack).
//
// The sync acknowledgements from storage are stashed in Replica.localMsgs and
// delivered back to RawNode by deliverLocalRaftMsgsRaftMuLockedReplicaMuLocked
// at the next opportunity.
//
// TODO(pav-kv): move and describe raft log compaction lifecycle code here.

// stateRaftMuLocked returns the current raft log storage state.
func (r *replicaLogStorage) stateRaftMuLocked() logstore.RaftState {
	return logstore.RaftState{
		LastIndex: r.shMu.lastIndexNotDurable,
		LastTerm:  r.shMu.lastTermNotDurable,
		ByteSize:  r.shMu.raftLogSize,
	}
}

// appendRaftMuLocked carries out a raft log append, and returns the new raft
// log storage state.
func (r *replicaLogStorage) appendRaftMuLocked(
	ctx context.Context, app logstore.MsgStorageAppend, stats *logstore.AppendStats,
) (logstore.RaftState, error) {
	state := r.stateRaftMuLocked()
	cb := (*replicaSyncCallback)(r)
	return r.raftMu.logStorage.StoreEntries(ctx, state, app, cb, stats)
}

// updateStateRaftMuLockedMuLocked updates the in-memory reflection of the raft
// log storage state.
func (r *replicaLogStorage) updateStateRaftMuLockedMuLocked(state logstore.RaftState) {
	r.shMu.lastIndexNotDurable = state.LastIndex
	r.shMu.lastTermNotDurable = state.LastTerm
	r.shMu.raftLogSize = state.ByteSize
}

// updateLogSize recomputes the raft log size, and updates Replica's in-memory
// knowledge about this size. Returns the computed log size.
//
// Replica.{raftMu,mu} must not be held.
func (r *replicaLogStorage) updateLogSize(ctx context.Context) (int64, error) {
	// We need to hold raftMu both to access the sideloaded storage and to make
	// sure concurrent raft activity doesn't foul up our update to the cached
	// in-memory values.
	r.raftMu.Lock()
	defer r.raftMu.Unlock()

	size, err := r.raftMu.logStorage.ComputeSize(ctx)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shMu.raftLogSize = size
	r.shMu.raftLogLastCheckSize = size
	r.shMu.raftLogSizeTrusted = true
	return size, nil
}

// --- Truncation ---

// The truncated state of a replica determines where its Raft log starts (by
// giving the last index that was already deleted). It's unreplicated -- it can
// differ between replicas at the same applied index. This divergence primarily
// occurs through snapshots that contain no log entries; the truncated index in
// the snapshot is set to equal the applied index it was generated from. The
// truncation itself then is a purely replicated side effect.
//
// Updates to the HardState are sent out by a leaseholder truncating the log
// based on its local knowledge. For example, the leader might have a log
// 10..100 and truncates to 50, and will send out a TruncatedState with Index 50
// to that effect. However, some replicas may not even have log entries that
// old and must make sure to ignore this update to the truncated state, as it
// would otherwise clobber their "newer" truncated state. The truncated state
// provided by the leader then is merely a suggested one -- we could ignore it
// and still be correct.
//
// We also rely on log truncations happening in the apply loop -- this makes
// sure that a truncation does not remove entries to be applied that we haven't
// yet. Since a truncation only ever removes committed log indexes, and after
// choosing the index gets proposed, the truncation command itself will be
// assigned an index higher than the one it could possibly remove. By the time
// the truncation itself is handled, the state machine will have applied all
// entries the truncation could possibly affect.
//
// The returned boolean tells the caller whether to apply the truncated state's
// side effects, which means replacing the in-memory TruncatedState and applying
// the associated RaftLogDelta. It is usually expected to be true, but may not
// be for the first truncation after on a replica that recently received a
// snapshot.

// truncateRaftMuLocked adds a raft log truncation to the given batch. Returns
// true if the batch has been updated, and the truncation can be applied.
//
// When the batch is written, the truncation should be finalized by calling
// updateTruncStateRaftMuLocked.
func (r *replicaLogStorage) truncateRaftMuLocked(
	ctx context.Context, newTruncState *kvserverpb.RaftTruncatedState, readWriter storage.ReadWriter,
) (_apply bool, _ error) {
	return logstore.Compact(ctx, r.shMu.raftTruncState, newTruncState,
		r.raftMu.stateLoader.StateLoader, readWriter)
}

// updateTruncStateRaftMuLocked finalizes the raft log truncation, after the
// corresponding write batch has been written to storage. It updates the
// in-memory state with the new RaftTruncatedState and log size delta, and
// removes obsolete entries from the raft log cache and sideloaded storage.
//
// The sideloadIncluded flag specifies whether the raftLogDelta already includes
// the total size of the sideloaded entries. It is true in loosely coupled
// truncations stack, and false in the tightly coupled stack. The latter will
// eventually be removed.
//
// The isDeltaTrusted flag specifies whether the raftLogDelta has been correctly
// computed. The loosely coupled truncations stack sets it to false if, for
// example, it failed to account for the sideloaded entries. The tightly coupled
// truncations have correct stats (but excluding the sideloaded entries).
//
// TODO(pav-kv): this can be simplified further.
func (r *replicaLogStorage) updateTruncStateRaftMuLocked(
	ctx context.Context,
	t kvserverpb.RaftTruncatedState,
	expectedFirstIndexPreTruncation kvpb.RaftIndex,
	raftLogDelta int64,
	sideloadIncluded bool,
	isDeltaTrusted bool,
) {
	r.raftMu.AssertHeld()
	// TODO(pav-kv): add unit tests for this logic.
	expectedFirstIndexWasAccurate :=
		r.shMu.raftTruncState.Index+1 == expectedFirstIndexPreTruncation
	isRaftLogTruncationDeltaTrusted := isDeltaTrusted &&
		(expectedFirstIndexWasAccurate || expectedFirstIndexPreTruncation == 0)

	// TODO(pav-kv): we are updating the truncation state, but leaving the raft
	// log size at the previous value and update it in a different Replica.mu
	// section in handleRaftLogDeltaResult. This can confuse the truncations queue
	// to make decisions based on incorrect stats. We should fix this and other
	// log stats inconsistencies.
	r.mu.Lock()
	r.shMu.raftTruncState = t
	r.mu.Unlock()

	// Clear any entries in the Raft log entry cache for this range up
	// to and including the most recently truncated index.
	r.store.raftEntryCache.Clear(r.RangeID, t.Index+1)

	// Truncate the sideloaded storage. This is safe because the new truncated
	// state is synced. If it wasn't, a crash right after removing the sideloaded
	// entries could result in missing entries in the log.
	log.Eventf(ctx, "truncating sideloaded storage up to (and including) index %d", t.Index)
	// TODO(#93248): when the legacy caller of handleTruncatedStateResult is
	// removed, stop calculating the size of the removed files.
	// TODO(pav-kv): TruncateTo can end up removing leftover files that were
	// previously compacted out of the log. We don't clean up the leftover files
	// at start up. So the size computation here can return a larger delta and
	// lead to inconsistent raft log size tracking.
	size, _, err := r.raftMu.sideloaded.TruncateTo(ctx, t.Index+1)
	if err != nil {
		// We don't *have* to remove these entries for correctness. Log a
		// loud error, but keep humming along.
		log.Errorf(ctx, "while removing sideloaded files during log truncation: %+v", err)
	}
	// NB: we don't sync the sideloaded entry files removal here for performance
	// reasons.
	//
	// TODO(pav-kv): If a crash occurs between syncing the truncated state and the
	// TruncateTo removing the files, there will be dangling files at the next
	// start. We should clean them up at startup.

	if !sideloadIncluded {
		raftLogDelta -= size
	}
	r.updateLogSizeWithDelta(raftLogDelta, isRaftLogTruncationDeltaTrusted)
}

// TODO(pav-kv): consolidate with other truncated state updates.
func (r *replicaLogStorage) updateLogSizeWithDelta(delta int64, isDeltaTrusted bool) {
	r.raftMu.AssertHeld()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shMu.raftLogSize += delta
	r.shMu.raftLogLastCheckSize += delta
	// Ensure raftLog{,LastCheck}Size is not negative since it isn't persisted
	// between server restarts.
	if r.shMu.raftLogSize < 0 {
		r.shMu.raftLogSize = 0
	}
	if r.shMu.raftLogLastCheckSize < 0 {
		r.shMu.raftLogLastCheckSize = 0
	}
	if !isDeltaTrusted {
		r.shMu.raftLogSizeTrusted = false
	}
}
