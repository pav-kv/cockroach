// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvstorage

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage/wag"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage/wag/wagpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/rditer"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/stateloader"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/errors"
)

type WAGBuilder struct {
	eng   *Engine
	Batch Batch

	addr   wagpb.Addr
	event  wagpb.ReplicaEvent
	events []wagpb.ReplicaEvent
}

func (e *Engine) MakeWAGBuilder() WAGBuilder {
	return WAGBuilder{eng: e, Batch: e.NewBatch()}
}

// CreateUninitialized creates an uninitialized replica in storage.
//
// Returns kvpb.RaftGroupDeletedError if this replica can not be created because
// it has been deleted.
func (w *WAGBuilder) CreateUninitialized(ctx context.Context, id roachpb.FullReplicaID) error {
	sl := stateloader.Make(id.RangeID)
	// Before creating the replica, see if there is a tombstone which would
	// indicate that this replica has been removed.
	// TODO(pav-kv): should also check that there is no existing replica, i.e.
	// ReplicaID load should find nothing.
	if ts, err := sl.LoadRangeTombstone(ctx, w.eng.state); err != nil {
		return err
	} else if id.ReplicaID < ts.NextReplicaID {
		return &kvpb.RaftGroupDeletedError{}
	}

	// TODO(pav-kv): assert that the event is empty.
	w.addr = wagpb.Addr{RangeID: id.RangeID, ReplicaID: id.ReplicaID, Index: 0}
	w.event = wagpb.ReplicaEvent{Type: wagpb.EventType_EventCreate, RangeID: id.RangeID}

	// Write the RaftReplicaID for this replica. This is the only place in the
	// CockroachDB code that we are creating a new *uninitialized* replica.
	//
	// Before this point, raft and state machine state of this replica are
	// non-existent. The only RangeID-specific key that can be present is the
	// RangeTombstone inspected above.
	_ = CreateUninitReplicaTODO
	if err := sl.SetRaftReplicaID(ctx, w.Batch.state, id.ReplicaID); err != nil {
		return err
	}
	// TODO(pav-kv): make sure that storage invariants for this uninitialized
	// replica hold.
	return nil
}

func (w *WAGBuilder) Apply(id roachpb.FullReplicaID, index kvpb.RaftIndex) {
	// TODO(pav-kv): assert that the event is empty.
	w.addr = wagpb.Addr{RangeID: id.RangeID, ReplicaID: id.ReplicaID, Index: index}
	w.event = wagpb.ReplicaEvent{RangeID: id.RangeID, Type: wagpb.EventType_EventApply}
}

func (w *WAGBuilder) Destroy(
	ctx context.Context, info DestroyReplicaInfo, next roachpb.ReplicaID, opts ClearRangeDataOptions,
) error {
	if info.ReplicaID >= next {
		return errors.AssertionFailedf("replica %v must not survive its own tombstone", info.FullReplicaID)
	}

	sl := stateloader.Make(info.RangeID)
	// Assert that the replica ID in storage matches the in-memory one.
	if id, err := sl.LoadRaftReplicaID(ctx, w.Batch.state); err != nil {
		return err
	} else if repID := id.ReplicaID; repID != info.ReplicaID {
		return errors.AssertionFailedf("replica %v has a mismatching ID %d", info.FullReplicaID, repID)
	}
	// Assert that the tombstone moves strictly forward. Failure to do so
	// indicates that something is going wrong in the replica lifecycle.
	if ts, err := sl.LoadRangeTombstone(ctx, w.Batch.state); err != nil {
		return err
	} else if ts.NextReplicaID >= next {
		return errors.AssertionFailedf(
			"cannot rewind tombstone from %d to %d", ts.NextReplicaID, next)
	}

	// replicated rangeID
	//	- full
	// unreplicated rangeID
	// 	- RangeTombstoneKey (sm, overwrite)
	// 	- RaftHardStateKey (raft, delete)
	// 	- RaftLogKey       (raft, delete or partially delete+WAG)
	// 	- RaftReplicaIDKey (sm, delete)
	// 	- RaftTruncatedStateKey (raft, delete)
	// 	- RangeLastReplicaGCTimestampKey (?)
	// system (range descs etc)
	// lock table
	// user keys

	if opts.ClearReplicatedByRangeID {
		opts.ClearReplicatedByRangeID = false
		span := rditer.MakeRangeIDReplicatedSpan(info.RangeID)
		if err := storage.ClearRangeWithHeuristic(
			ctx, w.Batch.state, w.Batch.state,
			span.Key, span.EndKey, ClearRangeThresholdPointKeys,
		); err != nil {
			return err
		}
	}

	if opts.ClearUnreplicatedByRangeID {
		// TODO(pav-kv): make this clearing future proof. Right now, we manually
		// handle each possible key.

		opts.ClearUnreplicatedByRangeID = false
		// Save a tombstone to ensure that replica IDs never get reused.
		if err := sl.SetRangeTombstone(ctx, w.Batch.state, kvserverpb.RangeTombstone{
			NextReplicaID: next, // NB: NextReplicaID > 0
		}); err != nil {
			return err
		}
		if err := w.Batch.raft.ClearEngineKey(storage.EngineKey{
			Key: sl.RaftHardStateKey(),
		}, storage.ClearOptions{}); err != nil {
			return err
		}
		if err := storage.ClearRangeWithHeuristic(
			ctx, w.Batch.raft, w.Batch.raft,
			sl.RaftLogKey(info.Log.Last+1),
			keys.RaftLogPrefix(info.RangeID).PrefixEnd(),
			ClearRangeThresholdPointKeys,
		); err != nil {
			return err
		}
		if err := w.Batch.state.ClearEngineKey(storage.EngineKey{
			Key: sl.RaftReplicaIDKey(),
		}, storage.ClearOptions{}); err != nil {
			return err
		}
		if err := w.Batch.raft.ClearEngineKey(storage.EngineKey{
			Key: sl.RaftTruncatedStateKey(),
		}, storage.ClearOptions{}); err != nil {
			return err
		}
		if err := w.Batch.state.ClearEngineKey(storage.EngineKey{
			Key: sl.RangeLastReplicaGCTimestampKey(),
		}, storage.ClearOptions{}); err != nil {
			return err
		}
	}

	// 2.2. (optional) Clear replicated MVCC span.
	if !opts.ClearReplicatedBySpan.Equal(roachpb.RSpan{}) {
		if err := ClearRangeData(ctx, info.RangeID, w.Batch.state, w.Batch.state, ClearRangeDataOptions{
			ClearReplicatedBySpan: opts.ClearReplicatedBySpan,
		}); err != nil {
			return err
		}
		opts.ClearReplicatedBySpan = roachpb.RSpan{}
	}

	// TODO(pav-kv): assert that opts has been cleared, to be future proof.
	w.events = append(w.events, wagpb.ReplicaEvent{
		RangeID: info.RangeID,
		Type:    wagpb.EventType_EventDestroy,
	})
	return nil
}

func (w *WAGBuilder) Commit(sync bool) error {
	if !w.eng.isSep {
		return w.Batch.state.Commit(sync)
	}
	w.eng.AddRaft(&w.Batch)
	if err := wag.Write(w.Batch.raft, w.eng.seq.Next(1), wagpb.Node{
		Addr:     w.addr,
		Mutation: wagpb.Mutation{Batch: w.Batch.state.Repr()},
		Event:    w.event,
		Events:   w.events,
	}); err != nil {
		return errors.Wrapf(err, "wailed to write WAG node")
	}
	if err := w.Batch.raft.Commit(true /* sync */); err != nil {
		return errors.Wrap(err, "wailed to commit WAG node")
	}
	return w.Batch.state.Commit(false /* sync */)
}

func (w *WAGBuilder) Close() {
	w.Batch.Close()
	*w = WAGBuilder{}
}
