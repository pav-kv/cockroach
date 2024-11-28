// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/logstore"
)

func (r *replicaLogStorage) stateRaftMuLocked() logstore.RaftState {
	return logstore.RaftState{
		LastIndex: r.shMu.lastIndexNotDurable,
		LastTerm:  r.shMu.lastTermNotDurable,
		ByteSize:  r.shMu.raftLogSize,
	}
}

func (r *replicaLogStorage) appendRaftMuLocked(
	ctx context.Context, app logstore.MsgStorageAppend, stats *logstore.AppendStats,
) (logstore.RaftState, error) {
	state := r.stateRaftMuLocked()
	cb := (*replicaSyncCallback)(r)
	return r.raftMu.logStorage.StoreEntries(ctx, state, app, cb, stats)
}

func (r *replicaLogStorage) updateStateRaftMuLockedMuLocked(state logstore.RaftState) {
	r.shMu.lastIndexNotDurable = state.LastIndex
	r.shMu.lastTermNotDurable = state.LastTerm
	r.shMu.raftLogSize = state.ByteSize
}
