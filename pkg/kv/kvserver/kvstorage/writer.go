// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvstorage

import (
	"context"
	"math"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage/wag"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage/wag/wagpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/stateloader"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/errors"
)

type DualBatch struct {
	sm   storage.WriteBatch
	raft storage.WriteBatch
}

func (b *DualBatch) Close() {
	b.sm.Close()
	if b.raft != nil {
		b.raft.Close()
	}
}

type Store struct {
	id  roachpb.StoreID
	eng Engines
	ws  *wag.Seq
}

func (s *Store) Create(ctx context.Context, id roachpb.FullReplicaID) error {
	sl := stateloader.Make(id.RangeID)
	// Before creating the replica, see if there is a tombstone which would
	// indicate that this replica has been removed.
	if ts, err := sl.LoadRangeTombstone(ctx, s.eng.se); err != nil {
		return err
	} else if id.ReplicaID < ts.NextReplicaID {
		return &kvpb.RaftGroupDeletedError{}
	}

	// Write the RaftReplicaID for this replica. This is the only place in the
	// CockroachDB code that we are creating a new *uninitialized* replica.
	//
	// Before this point, raft and state machine state of this replica are
	// non-existent. The only RangeID-specific key that can be present is the
	// RangeTombstone inspected above.
	b := s.eng.se.NewWriteBatch()
	defer b.Close()
	if err := sl.SetRaftReplicaID(ctx, b, id.ReplicaID); err != nil {
		return err
	}
	// Write the WAG node.
	if err := wag.Write(s.eng.le, s.ws.Next(1), wagpb.Node{
		Addr:     wagpb.Addr{RangeID: id.RangeID, ReplicaID: id.ReplicaID},
		Type:     wagpb.NodeType_NodeCreate,
		Mutation: wagpb.Mutation{Batch: b.Repr()},
		Create:   id.RangeID,
	}); err != nil {
		return err
	}
	// Write the state machine mutation.
	return b.Commit(true /* sync */)
}

/*
func DestroyReplica(
	ctx context.Context,
	rangeID roachpb.RangeID,
	reader storage.Reader,
	writer storage.Writer,
	nextReplicaID roachpb.ReplicaID,
	opts ClearRangeDataOptions,
*/

// DestroyRepl blah
//
// Keys:
//   - replicated RangeID-local   // state machine
//   - unreplicated RangeID-local // state&raft
//   - range-local
//   - lock/user
func DestroyRepl(
	ctx context.Context,
	r storage.Reader,
	e Engines,
	id roachpb.FullReplicaID,
	nextReplicaID roachpb.ReplicaID,
	opts ClearRangeDataOptions,
	ws *wag.Seq,
) error {
	b := e.se.NewWriteBatch()
	defer b.Close()

	if err := DestroyStateMachine(ctx, id.RangeID, r, b, nextReplicaID, opts); err != nil {
		return err
	}

	lb := e.le.NewWriteBatch()
	defer lb.Close()
	if err := DestroyRaft(ctx, id.RangeID, 0 /* FIXME */, r, lb); err != nil {
		return err
	}

	if err := wag.Write(lb, ws.Next(1), wagpb.Node{
		Addr:     wagpb.Addr{RangeID: id.RangeID, ReplicaID: id.ReplicaID, Index: math.MaxUint64},
		Type:     wagpb.NodeType_NodeDestroy,
		Mutation: wagpb.Mutation{Batch: b.Repr()},
	}); err != nil {
		return err
	}
	if err := lb.Commit(true /* sync */); err != nil {
		return err
	}
	return b.Commit(false /* sync */)
}

func DestroyStateMachine(
	ctx context.Context,
	rangeID roachpb.RangeID,
	reader storage.Reader,
	writer storage.Writer,
	nextReplicaID roachpb.ReplicaID,
	opts ClearRangeDataOptions,
) error {
	sl := stateloader.Make(rangeID)
	if id, err := sl.LoadRaftReplicaID(ctx, reader); err != nil {
		return err
	} else if nextReplicaID <= id.ReplicaID {
		return errors.AssertionFailedf("replica r%d/%d must not survive its own tombstone", rangeID, id.ReplicaID)
	}
	// Assert that the provided tombstone moves the existing one strictly forward.
	// Failure to do so indicates that something is going wrong in the replica
	// lifecycle.
	if ts, err := sl.LoadRangeTombstone(ctx, reader); err != nil {
		return err
	} else if nextReplicaID <= ts.NextReplicaID {
		return errors.AssertionFailedf("cannot rewind tombstone from %d to %d", ts.NextReplicaID, nextReplicaID)
	}

	// 2.1. RangeID-local replicated state.
	if err := ClearRangeData(ctx, rangeID, reader, writer, ClearRangeDataOptions{
		ClearReplicatedByRangeID: opts.ClearReplicatedByRangeID,
	}); err != nil {
		return err
	}
	// 2.1. RangeID-local unreplicated state.
	// Save a tombstone to ensure that replica IDs never get reused.
	if err := sl.SetRangeTombstone(ctx, writer, kvserverpb.RangeTombstone{
		NextReplicaID: nextReplicaID, // NB: nextReplicaID > 0
	}); err != nil {
		return err
	}
	if err := writer.ClearEngineKey(storage.EngineKey{
		Key: sl.RaftReplicaIDKey(),
	}, storage.ClearOptions{}); err != nil {
		return err
	}
	if err := writer.ClearEngineKey(storage.EngineKey{
		Key: sl.RangeLastReplicaGCTimestampKey(),
	}, storage.ClearOptions{}); err != nil {
		return err
	}
	// 2.2. (optional) Clear replicated MVCC span.
	return ClearRangeData(ctx, rangeID, reader, writer, ClearRangeDataOptions{
		ClearReplicatedBySpan: opts.ClearReplicatedBySpan,
		MustUseClearRange:     opts.MustUseClearRange,
	})
}

func DestroyRaft(
	ctx context.Context,
	rangeID roachpb.RangeID,
	applied kvpb.RaftIndex,
	r storage.Reader,
	w storage.Writer,
) error {
	// Clear raft HardState.
	buf := keys.MakeRangeIDPrefixBuf(rangeID)
	if err := w.ClearEngineKey(storage.EngineKey{
		Key: buf.RaftHardStateKey(),
	}, storage.ClearOptions{}); err != nil {
		return err
	}
	// Clear raft log at indices > after.
	if err := storage.ClearRangeWithHeuristic(
		ctx, r, w,
		keys.RaftLogKey(rangeID, applied+1),
		buf.RaftLogPrefix().PrefixEnd(),
		ClearRangeThresholdPointKeys,
	); err != nil {
		return err
	}
	// Clear RaftTruncatedState.
	return w.ClearEngineKey(storage.EngineKey{
		Key: buf.RaftTruncatedStateKey(),
	}, storage.ClearOptions{})
}
