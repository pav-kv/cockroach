// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvstorage

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage/wag"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage/wag/wagpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/stateloader"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
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
