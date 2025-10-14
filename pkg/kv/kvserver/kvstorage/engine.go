// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvstorage

import (
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvstorage/wag"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
)

// EnableSepStorage controls whether the separated storage is enabled. This is
// an experimental feature that can be true only in test builds for now.
var EnableSepStorage = envutil.EnvOrDefaultBool(
	"COCKROACH_ENABLE_SEPARATED_STORAGE", false,
) && buildutil.CrdbTestBuild

// Engine encapsulates the KV storage engine that allows accessing the state
// machine and raft state. It supports both combined and logically/physically
// separated engines (depending on whether the underlying storage engines are
// the same or different).
type Engine struct {
	state storage.Engine // state machine engine
	raft  storage.Engine // raft engine
	isSep bool           // specifies whether the engines are separated
	seq   wag.Seq
}

// MakeEngine returns an instance of Engine.
func MakeEngine(state, raft storage.Engine) Engine {
	if !EnableSepStorage && state != raft {
		panic("engines must be the same")
	}
	return Engine{state: state, raft: raft, isSep: EnableSepStorage}
}

// NewBatch creates a new write batch. By default, the batch only accepts
// mutation to the state machine. Transactions across the two engines must call
// AddRaft at least once, to allow writes to the raft engine.
func (e *Engine) NewBatch() Batch {
	return Batch{state: e.state.NewBatch()}
}

// AddRaft extends the batch with the capability of mutating the raft state. In
// the separated engines mode, the raft batch is distinct from the state machine
// batch.
func (e *Engine) AddRaft(b *Batch) {
	if b.raft != nil {
		return
	} else if e.isSep {
		b.raft = e.raft.NewBatch()
	} else {
		b.raft = b.state
	}
}

type Reader struct {
	state storage.Reader
	raft  storage.Reader
}

// Batch encapsulates the KV storage engine batch that allows writing to the
// state machine and (optionally) raft state.
type Batch struct {
	state storage.Batch // state machine batch
	raft  storage.Batch // raft state batch
}

func (b *Batch) IsSeparated() bool {
	return b.raft != nil && b.raft != b.state
}

// Close ends the lifetime of the batch and clears its resources.
func (b *Batch) Close() {
	if b.state == nil {
		return
	}
	// Close the raft batch only if it is separated from the state machine batch,
	// to avoid double Close.
	if b.raft != nil && b.raft != b.state {
		b.raft.Close()
	}
	b.raft = nil
	b.state.Close()
	b.state = nil
}

// TODO returns the "unified" batch. To be replaced by state machine and raft
// batch access.
func (b *Batch) TODO() storage.Batch {
	if buildutil.CrdbTestBuild && b.raft != nil && b.raft != b.state {
		panic("separated engines are not supported")
	}
	return b.state
}
