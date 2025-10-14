// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvstorage

import "github.com/cockroachdb/cockroach/pkg/storage"

// Engine encapsulates the KV storage engine that allows writing to the state
// machine and raft state.
type Engine interface {
	// NewBatch creates a new write batch. By default, the batch only accepts
	// mutation to the state machine.
	NewBatch() Batch
	// AddRaft extends the batch with the capability of mutating the raft state.
	AddRaft(*Batch)
}

// SingleEngine implements an Engine which combines state machine and raft state
// in a single storage engine.
type SingleEngine struct {
	e storage.Engine
}

// NewBatch creates a new combined write batch. By default, the batch only
// accepts mutation to the state machine.
func (s SingleEngine) NewBatch() Batch {
	return Batch{state: s.e.NewBatch()}
}

// AddRaft extends the batch with the capability of mutating the raft state. In
// the single-engine mode, it merely redirects all raft writes into the unified
// storage batch.
func (s SingleEngine) AddRaft(batch *Batch) {
	batch.raft = batch.state
}

// SeparatedEngine implements an Engine which distributes state machine and raft
// state mutations across two separate batches. It supports both logical and
// physical separation, depending on whether the underlying storage engines are
// the same or separated.
type SeparatedEngine struct {
	state storage.Engine // state machine engine
	raft  storage.Engine // raft engine
}

// NewBatch creates a new combined write batch. By default, the batch only
// accepts mutation to the state machine.
func (s SeparatedEngine) NewBatch() Batch {
	return Batch{state: s.state.NewBatch()}
}

// AddRaft extends the batch with the capability of mutating the raft state. In
// the separated engines mode, the raft batch is distinct from the state machine
// batch.
func (s SeparatedEngine) AddRaft(b *Batch) {
	if b.raft != nil {
		return
	}
	b.raft = s.raft.NewBatch()
}

// Batch encapsulates the KV storage engine batch that allows writing to the
// state machine and (optionally) raft state.
type Batch struct {
	state storage.Batch // state machine batch
	raft  storage.Batch // raft state batch
}

// Close ends the lifetime of the batch and clears its resources.
func (b Batch) Close() {
	// Close the raft batch only if it is separated from the state machine batch,
	// to avoid double Close.
	if b.raft != nil && b.raft != b.state {
		b.raft.Close()
	}
	b.raft = nil
	b.state.Close()
	b.state = nil
}
