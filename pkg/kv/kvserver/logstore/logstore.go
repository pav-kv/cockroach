// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Package logstore implements the Raft log storage.
package logstore

import (
	"context"
	"math"
	"math/rand"
	"slices"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/raftentry"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/raftlog"
	"github.com/cockroachdb/cockroach/pkg/raft"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/fs"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/iterutil"
	"github.com/cockroachdb/cockroach/pkg/util/metamorphic"
	"github.com/cockroachdb/crlib/crtime"
	"github.com/cockroachdb/errors"
)

// DisableSyncRaftLog disables raft log synchronization and can cause data loss.
var DisableSyncRaftLog = settings.RegisterBoolSetting(
	settings.SystemOnly,
	"kv.raft_log.disable_synchronization_unsafe",
	"disables synchronization of Raft log writes to persistent storage. "+
		"Setting to true risks data loss or data corruption on process or OS crashes. "+
		"This not only disables fsync, but also disables flushing writes to the OS buffer. "+
		"The setting is meant for internal testing only and SHOULD NOT be used in production.",
	envutil.EnvOrDefaultBool("COCKROACH_DISABLE_RAFT_LOG_SYNCHRONIZATION_UNSAFE", false),
	settings.WithName("kv.raft_log.synchronization.unsafe.disabled"),
	settings.WithUnsafe,
)

var enableNonBlockingRaftLogSync = settings.RegisterBoolSetting(
	settings.SystemOnly,
	"kv.raft_log.non_blocking_synchronization.enabled",
	"set to true to enable non-blocking synchronization on Raft log writes to "+
		"persistent storage. Setting to true does not risk data loss or data corruption "+
		"on server crashes, but can reduce write latency.",
	envutil.EnvOrDefaultBool("COCKROACH_ENABLE_RAFT_LOG_NON_BLOCKING_SYNCHRONIZATION", true),
)

// raftLogTruncationClearRangeThreshold is the number of entries at which Raft
// log truncation uses a Pebble range tombstone rather than point deletes. It is
// set high enough to avoid writing too many range tombstones to Pebble, but low
// enough that we don't do too many point deletes either (in particular, we
// don't want to overflow the Pebble write batch).
//
// In the steady state, Raft log truncation occurs when RaftLogQueueStaleSize
// (64 KB) or RaftLogQueueStaleThreshold (100 entries) is exceeded, so
// truncations are generally small. If followers are lagging, we let the log
// grow to RaftLogTruncationThreshold (16 MB) before truncating.
//
// 100k was chosen because it is unlikely to be hit in most common cases,
// keeping the number of range tombstones low, but will trigger when Raft logs
// have grown abnormally large. RaftLogTruncationThreshold will typically not
// trigger it, unless the average log entry is <= 160 bytes. The key size is ~16
// bytes, so Pebble point deletion batches will be bounded at ~1.6MB.
var raftLogTruncationClearRangeThreshold = kvpb.RaftIndex(metamorphic.ConstantWithTestRange(
	"raft-log-truncation-clearrange-threshold", 100000 /* default */, 1 /* min */, 1e6 /* max */))

// RaftState stores information about the last entry and the size of the log.
type RaftState struct {
	LastIndex kvpb.RaftIndex
	LastTerm  kvpb.RaftTerm
	ByteSize  int64
}

// EntryStats contains stats about the appended log slice.
type EntryStats struct {
	RegularEntries    int
	RegularBytes      int64
	SideloadedEntries int
	SideloadedBytes   int64
}

// AppendStats describes a completed log storage append operation.
type AppendStats struct {
	// Prepare contains the duration of preparing the write. Includes the time to
	// store the sideloaded entries, and to prepare the Pebble write batch.
	Prepare time.Duration

	EntryStats
	Pebble PebbleStats
}

type PebbleStats struct {
	Bytes int64

	// WriteDur is the time it took to write the batch to Pebble. Does not account
	// for the sync waiting time if the write does not require sync, or the sync
	// is non-blocking.
	WriteDur time.Duration
	// CommitStats is set only when !NonBlocking, which means almost never, since
	// kv.raft_log.non_blocking_synchronization.enabled defaults to true.
	CommitStats storage.BatchCommitStats

	Sync        bool
	NonBlocking bool
}

// WriteStats contains stats about a write to raft storage.
type WriteStats struct {
	// CommitDur is the time it took to commit and sync the batch to Pebble.
	CommitDur time.Duration
	storage.BatchCommitStats
}

// LogStore is a stub of a separated Raft log storage.
type LogStore struct {
	RangeID     roachpb.RangeID
	Engine      storage.Engine
	Sideload    SideloadStorage
	StateLoader StateLoader // used only for writes under raftMu
	SyncWaiter  *SyncWaiterLoop
	EntryCache  *raftentry.Cache
	Settings    *cluster.Settings

	DisableSyncLogWriteToss bool // for testing only
}

// SyncCallback is a callback that is notified when a raft log write has been
// durably committed to disk.
//
// The function is handed the struct containing messages that are associated
// with the MsgStorageAppend that triggered the fsync.
//
// commitStats is populated iff this was a non-blocking sync.
type SyncCallback interface {
	OnLogSync(context.Context, raft.StorageAppendAck, WriteStats)
}

func newStoreEntriesBatch(eng storage.Engine) storage.Batch {
	// Use an unindexed batch because we don't need to read our writes, and
	// it is more efficient.
	return eng.NewUnindexedBatch()
}

// StoreEntries persists newly appended Raft log Entries to the log storage,
// then calls the provided callback with the input's response messages (if any)
// once the entries are durable. The durable log write may or may not be
// blocking (and therefore the callback may or may not be called synchronously),
// depending on the kv.raft_log.non_blocking_synchronization.enabled cluster
// setting. Either way, the effects of the log append will be immediately
// visible to readers of the Engine.
//
// Accepts the state of the log before the operation, returns the state after.
// Persists HardState atomically with, or strictly after Entries.
func (s *LogStore) StoreEntries(
	ctx context.Context, state RaftState, app raft.StorageAppend, cb SyncCallback, stats *AppendStats,
) (RaftState, error) {
	wb, err := s.prepareBatch(ctx, state, app)
	if err != nil {
		return state, err
	}
	if wb.sd.nonBlocking {
		err = s.writeBatchAsync(ctx, &wb, cb)
	} else {
		err = s.writeBatch(ctx, &wb, cb)
	}
	if err != nil {
		return state, err
	}
	if err := s.finalizeBatch(ctx, &wb); err != nil {
		return state, err
	}
	*stats = wb.stats
	return wb.state, nil
}

type writeBatch struct {
	batch    storage.Batch
	entries  []raftpb.Entry
	ack      raft.StorageAppendAck
	prevLast kvpb.RaftIndex
	state    RaftState
	sd       syncDecision
	stats    AppendStats
}

// release de-references all the data held by this batch.
func (wb *writeBatch) release() {
	wb.batch = nil
	// wb.entries = nil
	wb.ack = raft.StorageAppendAck{}
}

// close closes this batch and releases all data referenced by it.
func (wb *writeBatch) close() {
	wb.batch.Close()
	wb.release()
}

func (s *LogStore) prepareBatch(
	ctx context.Context, state RaftState, app raft.StorageAppend,
) (_ writeBatch, err error) {
	wb := writeBatch{
		ack:      app.Ack(),
		prevLast: state.LastIndex,
		sd:       s.syncDecision(state.LastIndex, app),
	}
	// Filter out and store/sync the new sideloaded entries first, and make the
	// corresponding entries in the slice thin. Account for the added bytes and
	// time spent.
	var thinEntries []raftpb.Entry
	if ln := len(app.Entries); ln > 0 {
		begin := crtime.NowMono()
		wb.entries = app.Entries
		entries, entryStats, err := MaybeSideloadEntries(ctx, app.Entries, s.Sideload)
		if err != nil {
			return writeBatch{}, errors.Wrap(err, "during sideloading")
		}
		thinEntries = entries
		wb.stats.EntryStats = entryStats
		wb.stats.Prepare = begin.Elapsed()
		state.ByteSize += entryStats.SideloadedBytes
	}

	wb.batch = newStoreEntriesBatch(s.Engine)
	defer func() {
		if err != nil {
			wb.close()
		}
	}()

	builder := writeBuilder{rw: wb.batch, sl: s.StateLoader, rs: state}
	if err := builder.setHardState(ctx, app.HardState); err != nil {
		return writeBatch{}, errors.Wrap(err, "during setHardState")
	}
	if err := builder.logAppend(ctx, thinEntries); err != nil {
		return writeBatch{}, errors.Wrap(err, "during logAppend")
	}
	wb.state = builder.rs

	wb.stats.Pebble = PebbleStats{
		Bytes:       int64(wb.batch.Len()),
		Sync:        wb.sd.wantsSync,
		NonBlocking: wb.sd.nonBlocking,
	}
	return wb, nil
}

func (s *LogStore) writeBatch(ctx context.Context, wb *writeBatch, cb SyncCallback) error {
	defer wb.close()
	begin := crtime.NowMono()
	if err := wb.batch.Commit(wb.sd.willSync); err != nil {
		return errors.Wrap(err, "while committing batch")
	}
	wb.stats.Pebble.WriteDur = begin.Elapsed()
	wb.stats.Pebble.CommitStats = wb.batch.CommitStats()
	if wb.sd.wantsSync {
		cb.OnLogSync(ctx, wb.ack, WriteStats{CommitDur: wb.stats.Pebble.WriteDur})
	}
	return nil
}

// writeBatchAsync writes the prepared raft storage batch, and hands it over to
// SyncWaiter. When the write is synced, SyncWaiter will close the batch and
// invoke the passed in sync callback.
func (s *LogStore) writeBatchAsync(ctx context.Context, wb *writeBatch, cb SyncCallback) error {
	// Apply the batched updates to the engine and initiate a synchronous disk
	// write, but don't wait for the write to complete.
	begin := crtime.NowMono()
	if err := wb.batch.CommitNoSyncWait(); err != nil {
		return errors.Wrap(err, "while committing batch without sync wait")
	}
	wb.stats.Pebble.WriteDur = begin.Elapsed()
	// Enqueue that waiting on the SyncWaiterLoop, who will signal the callback
	// when the write completes.
	waiterCallback := nonBlockingSyncWaiterCallbackPool.Get().(*nonBlockingSyncWaiterCallback)
	*waiterCallback = nonBlockingSyncWaiterCallback{
		ctx:            ctx,
		cb:             cb,
		onDone:         wb.ack,
		batch:          wb.batch,
		logCommitBegin: begin,
	}
	s.SyncWaiter.enqueue(ctx, wb.batch, waiterCallback)
	wb.release()
	return nil
}

func (s *LogStore) finalizeBatch(ctx context.Context, wb *writeBatch) error {
	// Update raft log entry cache. We clear any older, uncommitted log entries
	// and cache the latest ones.
	//
	// In the blocking log sync case, these entries are already durable. In the
	// non-blocking case, these entries have been written to the pebble engine (so
	// reads of the engine will see them), but they are not yet be durable. This
	// means that the entry cache can lead the durable log. This is allowed by
	// etcd/raft, which maintains its own tracking of entry durability by
	// splitting its log into an unstable portion for entries that are not known
	// to be durable and a stable portion for entries that are known to be
	// durable.
	s.EntryCache.Add(s.RangeID, wb.entries, true /* truncate */)

	if !wb.sd.overwriting {
		return nil
	}
	// We may have just overwritten parts of the log which contain sideloaded
	// SSTables from a previous term (and perhaps discarded some entries that we
	// didn't overwrite). Remove any such leftover on-disk payloads (we can do
	// that now because we've committed the deletion just above).
	//
	// TODO(pav-kv): this logic is incorrect. There can be entries from lower
	// terms that got removed.
	firstPurge := kvpb.RaftIndex(wb.entries[0].Index) // first new entry written
	purgeTerm := kvpb.RaftTerm(wb.entries[0].Term - 1)
	lastPurge := wb.prevLast // old end of the log, include in deletion
	purgedSize, err := maybePurgeSideloaded(ctx, s.Sideload, firstPurge, lastPurge, purgeTerm)
	if err != nil {
		return errors.Wrap(err, "while purging sideloaded storage")
	}
	wb.state.ByteSize -= purgedSize
	if wb.state.ByteSize < 0 {
		// Might have gone negative if node was recently restarted.
		wb.state.ByteSize = 0
	}
	return nil
}

type syncDecision struct {
	// overwriting is true if the storage write overwrites some log entries.
	overwriting bool
	// wantsSync is true if there are raft events contingent on storage sync, such
	// as sending vote or append acknowledgements to the proposer.
	wantsSync bool
	// willSync is true if the write will be synced.
	willSync bool
	// nonBlocking is true if the sync will be non-blocking.
	nonBlocking bool
}

func (s *LogStore) syncDecision(prevLast kvpb.RaftIndex, app raft.StorageAppend) syncDecision {
	sd := syncDecision{
		overwriting: len(app.Entries) > 0 && kvpb.RaftIndex(app.Entries[0].Index) <= prevLast,
	}
	// We want a timely sync in two cases:
	//	1. There are raft messages (e.g. MsgVoteResp and MsgAppResp) conditional
	//	on this write being durable. Sending these messages is on the critical path
	//	for writes/consensus.
	//	2. The log append overwrites some entries. Since there can be sideloaded
	//	entries to be removed, we must sync before doing so. This is an internal
	//	detail here: we could instead do a non-blocking sync and remove sideloaded
	//	entries asynchronously. See #136416.
	sd.wantsSync = app.MustSync() || sd.overwriting
	sd.willSync = sd.wantsSync && !DisableSyncRaftLog.Get(&s.Settings.SV)
	// Use the non-blocking log sync path if we are performing a storage sync.
	sd.nonBlocking = sd.willSync &&
		// And the cluster setting is enabled.
		enableNonBlockingRaftLogSync.Get(&s.Settings.SV) &&
		// And we are not overwriting any previous log entries. If we are
		// overwriting, we may need to purge the sideloaded SSTables associated with
		// overwritten entries. This must be performed after the corresponding
		// entries are durably replaced and it's easier to ensure proper ordering
		// using a blocking log sync. This is a rare case, so it's not worth
		// optimizing for.
		!sd.overwriting &&
		// Also, randomly disable non-blocking sync in test builds to exercise the
		// interleaved blocking and non-blocking syncs (unless the testing knobs
		// disable this randomization explicitly).
		!(buildutil.CrdbTestBuild && !s.DisableSyncLogWriteToss && rand.Intn(2) == 0)
	return sd
}

// finalizeWrite updates the log state after the storage write is done. Removes
// obsolete sideloaded entries and updates the entry cache. Returns a non-zero
// log size delta if any sideloaded entries have been removed.
func (s *LogStore) finalizeWrite(
	ctx context.Context, entries []raftpb.Entry, prevLastIndex kvpb.RaftIndex,
) (sizeDelta int64, _ error) {
	if len(entries) == 0 {
		return 0, nil
	}
	if first := kvpb.RaftIndex(entries[0].Index); first <= prevLastIndex {
		// We may have just overwritten parts of the log which contain sideloaded
		// SSTables from a previous term (and perhaps discarded some entries that we
		// didn't overwrite). Remove any such leftover on-disk payloads (we can do
		// that now because we've committed the deletion just above).
		//
		// TODO(#136416): this logic is incorrect. There can be obsoleted entries
		// from lower terms that are still remaining. We should move this clearing
		// out of the critical path, and fix the related log size tracking bugs and
		// dangling sideloaded files.
		// A quick fix is to prepare a list of to-be-removed sideloaded files in
		// advance. After writing / syncing new entries, remove these files.
		purgeTerm := kvpb.RaftTerm(entries[0].Term - 1)
		purgedSize, err := maybePurgeSideloaded(ctx, s.Sideload, first, prevLastIndex, purgeTerm)
		if err != nil {
			return 0, errors.Wrap(err, "while purging sideloaded storage")
		}
		sizeDelta = -purgedSize
	}

	// Update raft log entry cache. We clear any older, uncommitted log entries
	// and cache the latest ones.
	//
	// In the blocking log sync case, these entries are already durable. In the
	// non-blocking case, these entries have been written to the pebble engine (so
	// reads of the engine will see them), but they are not yet be durable. This
	// means that the entry cache can lead the durable log. This is allowed by
	// raft, which maintains its own tracking of entry durability by splitting its
	// log into an unstable portion for entries that are not known to be durable
	// and a stable portion for entries that are known to be durable.
	//
	// TODO(pav-kv): for safety, decompose this update into two steps: before
	// writing the storage batch, truncate the suffix of the cache if entries are
	// overwritten; after the write, append new entries to the cache. Currently,
	// the cache can contain stale entries while storage is already updated, and
	// the only hope is that nobody tries to read it under Replica.mu only.
	s.EntryCache.Add(s.RangeID, entries, true /* truncate */)
	return sizeDelta, nil
}

// nonBlockingSyncWaiterCallback packages up the callback that is handed to the
// SyncWaiterLoop during a non-blocking Raft log sync. Structuring the callback
// as a struct with a method instead of an anonymous function avoids individual
// fields escaping to the heap. It also provides the opportunity to pool the
// callback.
type nonBlockingSyncWaiterCallback struct {
	// Used to run SyncCallback.
	ctx    context.Context
	cb     SyncCallback
	onDone raft.StorageAppendAck
	// Used to extract stats. This is the batch that has been synced.
	batch storage.WriteBatch
	// Used to measure raft storage write/sync latency.
	logCommitBegin crtime.Mono
}

// run is the callback's logic. It is executed on the SyncWaiterLoop goroutine.
func (cb *nonBlockingSyncWaiterCallback) run() {
	cb.cb.OnLogSync(cb.ctx, cb.onDone, WriteStats{
		CommitDur:        cb.logCommitBegin.Elapsed(),
		BatchCommitStats: cb.batch.CommitStats(),
	})
	cb.release()
}

func (cb *nonBlockingSyncWaiterCallback) release() {
	*cb = nonBlockingSyncWaiterCallback{}
	nonBlockingSyncWaiterCallbackPool.Put(cb)
}

var nonBlockingSyncWaiterCallbackPool = sync.Pool{
	New: func() interface{} { return new(nonBlockingSyncWaiterCallback) },
}

var logAppendPool = sync.Pool{
	New: func() interface{} {
		return new(struct {
			roachpb.Value
			enginepb.MVCCStats
		})
	},
}

type writeBuilder struct {
	rw storage.ReadWriter
	sl StateLoader
	rs RaftState
}

func (w *writeBuilder) setHardState(ctx context.Context, hs raftpb.HardState) error {
	if raft.IsEmptyHardState(hs) {
		return nil
	}
	// NB: Note that without additional safeguards, it's incorrect to write the
	// HardState before appending m.Entries. When catching up, a follower will
	// receive Entries that are immediately Committed in the same Ready. If we
	// persist the HardState but happen to lose the Entries, assertions can be
	// tripped.
	//
	// We have both in the same batch, so there's no problem. If that ever
	// changes, we must write and sync the Entries before the HardState.
	return w.sl.SetHardState(ctx, w.rw, hs)
}

// logAppend adds the given entries to the raft log. Updates the RaftState for
// this builder. It's the caller's responsibility to maintain exclusive access
// to the raft log for the duration of the method call.
//
// logAppend is intentionally oblivious to the existence of sideloaded
// proposals. They are managed by the caller, including cleaning up obsolete
// on-disk payloads in case the log tail is replaced.
func (w *writeBuilder) logAppend(ctx context.Context, entries []raftpb.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	// NB: the Value and MVCCStats lifetime is this function, so we coalesce their
	// allocation into the same pool.
	// TODO(pavelkalinnikov): figure out why they escape into the heap, and find a
	// way to avoid the pool.
	v := logAppendPool.Get().(*struct {
		roachpb.Value
		enginepb.MVCCStats
	})
	defer logAppendPool.Put(v)
	value, diff := &v.Value, &v.MVCCStats
	value.RawBytes = value.RawBytes[:0]
	diff.Reset()

	opts := storage.MVCCWriteOptions{Stats: diff, Category: fs.ReplicationReadCategory}
	raftLogPrefix := w.sl.RaftLogPrefix()
	for i := range entries {
		ent := &entries[i]
		key := keys.RaftLogKeyFromPrefix(raftLogPrefix, kvpb.RaftIndex(ent.Index))

		if err := value.SetProto(ent); err != nil {
			return err
		}
		value.InitChecksum(key)
		var err error
		if kvpb.RaftIndex(ent.Index) > w.rs.LastIndex {
			_, err = storage.MVCCBlindPut(ctx, w.rw, key, hlc.Timestamp{}, *value, opts)
		} else {
			_, err = storage.MVCCPut(ctx, w.rw, key, hlc.Timestamp{}, *value, opts)
		}
		if err != nil {
			return err
		}
	}

	newLast := kvpb.RaftIndex(entries[len(entries)-1].Index)
	// Delete any previously appended log entries which never committed.
	if prevLast := w.rs.LastIndex; prevLast > 0 {
		for i := newLast + 1; i <= prevLast; i++ {
			// Note that the caller is in charge of deleting any sideloaded payloads
			// (which they must only do *after* the batch has committed).
			_, _, err := storage.MVCCDelete(ctx, w.rw, keys.RaftLogKeyFromPrefix(raftLogPrefix, i),
				hlc.Timestamp{}, opts)
			if err != nil {
				return err
			}
		}
	}
	w.rs = RaftState{
		LastIndex: newLast,
		LastTerm:  kvpb.RaftTerm(entries[len(entries)-1].Term),
		ByteSize:  w.rs.ByteSize + diff.SysBytes,
	}
	return nil
}

// Compact prepares a write that removes entries (prev.Index, next.Index] from
// the raft log, and updates the truncated state to the next one. Does nothing
// if the interval is empty.
//
// TODO(#136109): make this a method of a write-through LogStore data structure,
// which is aware of the current log state.
func Compact(
	ctx context.Context,
	prev kvserverpb.RaftTruncatedState,
	next kvserverpb.RaftTruncatedState,
	loader StateLoader,
	writer storage.Writer,
) error {
	if next.Index <= prev.Index {
		// TODO(pav-kv): return an assertion failure error.
		return nil
	}
	// Truncate the Raft log from the entry after the previous truncation index to
	// the new truncation index. This is performed atomically with updating the
	// RaftTruncatedState so that the state of the log is consistent.
	prefixBuf := &loader.RangeIDPrefixBuf
	numTruncatedEntries := next.Index - prev.Index
	if numTruncatedEntries >= raftLogTruncationClearRangeThreshold {
		start := prefixBuf.RaftLogKey(prev.Index + 1).Clone()
		end := prefixBuf.RaftLogKey(next.Index + 1).Clone() // end is exclusive
		if err := writer.ClearRawRange(start, end, true, false); err != nil {
			return errors.Wrapf(err,
				"unable to clear truncated Raft entries for %+v after index %d",
				next, prev.Index)
		}
	} else {
		// NB: RangeIDPrefixBufs have sufficient capacity (32 bytes) to avoid
		// allocating when constructing Raft log keys (16 bytes).
		prefix := prefixBuf.RaftLogPrefix()
		for idx := prev.Index + 1; idx <= next.Index; idx++ {
			if err := writer.ClearUnversioned(
				keys.RaftLogKeyFromPrefix(prefix, idx),
				storage.ClearOptions{},
			); err != nil {
				return errors.Wrapf(err, "unable to clear truncated Raft entries for %+v at index %d",
					next, idx)
			}
		}
	}

	key := prefixBuf.RaftTruncatedStateKey()
	var value roachpb.Value
	if _, err := next.MarshalToSizedBuffer(value.AllocBytes(next.Size())); err != nil {
		return err
	}
	value.InitChecksum(key)

	if _, err := storage.MVCCBlindPut(
		ctx, writer, key, hlc.Timestamp{}, value, storage.MVCCWriteOptions{},
	); err != nil {
		return errors.Wrap(err, "unable to write RaftTruncatedState")
	}
	return nil
}

// ComputeSize computes the size (in bytes) of the raft log from the storage
// engine. This will iterate over the raft log and sideloaded files, so
// depending on the size of these it can be mildly to extremely expensive and
// thus should not be called frequently.
//
// TODO(#136358): we should be able to maintain this size incrementally, and not
// need scanning the log to re-compute it.
func (s *LogStore) ComputeSize(ctx context.Context) (int64, error) {
	prefix := keys.RaftLogPrefix(s.RangeID)
	prefixEnd := prefix.PrefixEnd()
	ms, err := storage.ComputeStats(ctx, s.Engine, prefix, prefixEnd, 0 /* nowNanos */)
	if err != nil {
		return 0, err
	}
	_, totalSideloaded, err := s.Sideload.Stats(ctx, kvpb.RaftSpan{Last: math.MaxUint64})
	if err != nil {
		return 0, err
	}
	return ms.SysBytes + totalSideloaded, nil
}

// LoadTerm returns the term of the entry at the given index for the specified
// range. The result is loaded from the storage engine if it's not in the cache.
// The valid range for index is [Compacted, LastIndex].
//
// There are 3 cases for when the term is not found: (1) the index has been
// compacted away, (2) index > LastIndex, or (3) there is a gap in the log. In
// the first case, we return ErrCompacted, and ErrUnavailable otherwise. Most
// callers never try to read indices above LastIndex, so an error means (3)
// which is a serious issue. But if the caller is unsure, they can check the
// LastIndex to distinguish.
//
// TODO(#132114): eliminate both ErrCompacted and ErrUnavailable.
func LoadTerm(
	ctx context.Context,
	rsl StateLoader,
	eng storage.Engine,
	rangeID roachpb.RangeID,
	eCache *raftentry.Cache,
	index kvpb.RaftIndex,
) (kvpb.RaftTerm, error) {
	entry, found := eCache.Get(rangeID, index)
	if found {
		return kvpb.RaftTerm(entry.Term), nil
	}

	reader := eng.NewReader(storage.StandardDurability)
	defer reader.Close()

	if err := raftlog.Visit(ctx, reader, rangeID, index, index+1, func(ent raftpb.Entry) error {
		if found {
			return errors.Errorf("found more than one entry in [%d,%d)", index, index+1)
		}
		found = true
		entry = ent
		return nil
	}); err != nil {
		return 0, err
	}

	if found {
		// Found an entry. Double-check that it has a correct index.
		if got, want := kvpb.RaftIndex(entry.Index), index; got != want {
			return 0, errors.Errorf("there is a gap at index %d, found entry #%d", want, got)
		}
		// Cache the entry except if it is sideloaded. We don't load/inline the
		// sideloaded entries here to keep the term fetching cheap.
		// TODO(pavelkalinnikov): consider not caching here, after measuring if it
		// makes any difference.
		typ, _, err := raftlog.EncodingOf(entry)
		if err != nil {
			return 0, err
		}
		if !typ.IsSideloaded() {
			eCache.Add(rangeID, []raftpb.Entry{entry}, false /* truncate */)
		}
		return kvpb.RaftTerm(entry.Term), nil
	}

	// Otherwise, the entry at the given index is not found. See the function
	// comment for how this case is handled.
	ts, err := rsl.LoadRaftTruncatedState(ctx, reader)
	if err != nil {
		return 0, err
	} else if index == ts.Index {
		return ts.Term, nil
	} else if index < ts.Index {
		return 0, raft.ErrCompacted
	}
	return 0, raft.ErrUnavailable
}

// LoadEntries loads a slice of consecutive log entries in [lo, hi), starting
// from lo. It inlines the sideloaded entries, and caches all the loaded
// entries. The size of the returned entries does not exceed maxSize, unless the
// first entry exceeds the limit (in which case it is returned regardless).
//
// The valid range for lo/hi is: Compacted < lo <= hi <= LastIndex+1.
//
// There are 3 cases for when an entry is not found: (1) the lo index has been
// compacted away, (2) hi > LastIndex+1, or (3) there is a gap in the log. In
// the first case, we return ErrCompacted, and ErrUnavailable otherwise. Most
// callers never try to read indices above LastIndex, so an error means (3)
// which is a serious issue. But if the caller is unsure, they can check the
// LastIndex to distinguish.
//
// The bytesAccount is used to account for and limit the loaded bytes. It can be
// nil when the accounting / limiting is not needed.
//
// TODO(#132114): eliminate both ErrCompacted and ErrUnavailable.
// TODO(pavelkalinnikov): return all entries we've read, consider maxSize a
// target size. Currently we may read one extra entry and drop it.
func LoadEntries(
	ctx context.Context,
	rsl StateLoader,
	eng storage.Engine,
	rangeID roachpb.RangeID,
	eCache *raftentry.Cache,
	sideloaded SideloadStorage,
	lo, hi kvpb.RaftIndex,
	maxBytes uint64,
	account *BytesAccount,
) (_ []raftpb.Entry, _cachedSize uint64, _loadedSize uint64, _ error) {
	if lo > hi {
		return nil, 0, 0, errors.Errorf("lo:%d is greater than hi:%d", lo, hi)
	}

	n := hi - lo
	if n > 100 {
		n = 100
	}
	ents := make([]raftpb.Entry, 0, n)
	ents, _, hitIndex, _ := eCache.Scan(ents, rangeID, lo, hi, maxBytes)

	// TODO(pav-kv): pass the sizeHelper to eCache.Scan above, to avoid scanning
	// the same entries twice, and computing their sizes.
	sh := sizeHelper{maxBytes: maxBytes, account: account}
	for i, entry := range ents {
		if sh.done || !sh.add(uint64(entry.Size())) {
			// Remove the remaining entries, and dereference the memory they hold.
			ents = slices.Delete(ents, i, len(ents))
			break
		}
	}
	// NB: if we couldn't get quota for all cached entries, return only a prefix
	// for which we got it. Even though all the cached entries are already in
	// memory, returning all of them would increase their lifetime, incur size
	// amplification when processing them, and risk reaching out-of-memory state.
	cachedSize := sh.bytes
	// Return results if the correct number of results came back, or we ran into
	// the max bytes limit, or reached the memory budget limit.
	if len(ents) == int(hi-lo) || sh.done {
		return ents, cachedSize, 0, nil
	}

	// Scan over the log to find the requested entries in the range [lo, hi),
	// stopping once we have enough.
	expectedIndex := hitIndex

	scanFunc := func(ent raftpb.Entry) error {
		// Exit early if we have any gaps or it has been compacted.
		if kvpb.RaftIndex(ent.Index) != expectedIndex {
			return iterutil.StopIteration()
		}
		expectedIndex++

		if typ, _, err := raftlog.EncodingOf(ent); err != nil {
			return err
		} else if typ.IsSideloaded() {
			if ent, err = MaybeInlineSideloadedRaftCommand(
				ctx, rangeID, ent, sideloaded, eCache,
			); err != nil {
				return err
			}
		}

		if sh.add(uint64(ent.Size())) {
			ents = append(ents, ent)
		}
		if sh.done {
			return iterutil.StopIteration()
		}
		return nil
	}

	reader := eng.NewReader(storage.StandardDurability)
	defer reader.Close()
	if err := raftlog.Visit(ctx, reader, rangeID, expectedIndex, hi, scanFunc); err != nil {
		return nil, 0, 0, err
	}
	eCache.Add(rangeID, ents, false /* truncate */)

	// Did the correct number of results come back? If so, we're all good.
	// Did we hit the size limits? If so, return what we have.
	if len(ents) == int(hi-lo) || sh.done {
		return ents, cachedSize, sh.bytes - cachedSize, nil
	}

	// Something went wrong, and we could not load enough entries.
	ts, err := rsl.LoadRaftTruncatedState(ctx, reader)
	if err != nil {
		return nil, 0, 0, err
	} else if lo <= ts.Index {
		// The requested lo index has already been truncated.
		return nil, 0, 0, raft.ErrCompacted
	}
	// We either have a gap in the log, or hi > LastIndex. Let the caller
	// distinguish if they need to.
	return nil, 0, 0, raft.ErrUnavailable
}
