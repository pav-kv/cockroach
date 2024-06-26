// This code has been modified from its original form by Cockroach Labs, Inc.
// All modifications are Copyright 2024 Cockroach Labs, Inc.
//
// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import pb "github.com/cockroachdb/cockroach/pkg/raft/raftpb"

// unstable is a suffix of the raft log not yet written to Storage. The "log"
// state can be represented by a snapshot, and/or a contiguous slice of entries.
//
// The possible states:
//  1. Both the snapshot and the entries logSlice are empty. This means the log
//     is fully in stable Storage.
//  2. The snapshot is empty, and the entries logSlice is non-empty. All entries
//     up to logSlice.prev are in Storage, and the logSlice is not yet written.
//  3. The snapshot is non-empty, and the logSlice is empty. The snapshot
//     represents the (0, lastIndex] log prefix being written.
//  4. Both the snapshot and logSlice are non-empty. The snapshot immediately
//     precedes the entries, i.e. the first entry is at snapshot.index + 1. This
//     state represents the (0, lastIndex] log prefix being written.
//
// The type serves two roles. First, it holds on to the latest snapshot / log
// entries until they are handed to storage for persistence (via Ready API) and
// subsequently acknowledged. This provides the RawNode with a consistent view
// on the latest state of the log: it can be constructed by combining the log
// prefix from Storage, and the suffix from unstable.
//
// Second, it supports the asynchronous log writes protocol. The snapshot and
// the entries are handed to storage in the order that guarantees consistency of
// the log. The unstable structure expects acknowledgments from the storage to
// arrive in the same order, and clears the snapshot and the entries from memory
// when it is safe to do so.
//
// TODO(pav-kv): describe the order guarantee and the write protocol when
// accTerm (logSlice.term) is integrated into the protocol.
//
// Note that the in-memory prefix of the log can contain entries at indices less
// than Storage.LastIndex(). This means that the next write to storage might
// need to truncate the log before persisting the new suffix. Such a situation
// happens when there is a leader change, and the new leader overwrites entries
// that haven't been committed yet.
type unstable struct {
	// snapshot is the incoming unstable snapshot, if any.
	//
	// Invariant: snapshot == nil ==> !snapInProgress
	// Invariant: snapshot != nil ==> snapshot.lastEntryID == logSlice.prev
	//
	// The last invariant enforces the order of handling a situation when there is
	// both a snapshot and entries. The snapshot must be acknowledged first,
	// before entries are acknowledged and offset moves forward.
	snapshot *pb.Snapshot

	// logSlice is the suffix of the raft log that is not yet written to storage.
	// If all the entries are stable, or covered by an in-flight snapshot, then
	// logSlice.entries is empty.
	//
	// Invariant: logSlice.term, a.k.a. the "last accepted term", is the term of
	// the leader whose append (either entries or snapshot) we accepted last. Our
	// log is consistent with the leader at this term.
	//
	// Invariant: logSlice.prev is the ID of the last persisted entry, or the ID
	// of the in-flight snapshot if snapshot != nil.
	logSlice

	// snapInProgress is true if the snapshot is being written to storage.
	//
	// Invariant: snapInProgress ==> snapshot != nil
	snapInProgress bool
	// inProgress is the index of the last entry in logSlice already being written
	// to storage.
	//
	// Invariant: logSlice.prev.index <= inProgress <= logSlice.lastIndex().
	// Invariant: inProgress > logSlice.prev.index ==> snapInProgress == true
	//
	// The last invariant enforces the order of handling a situation when there is
	// both a snapshot and entries. The snapshot must be sent to storage first, or
	// together with the entries.
	inProgress uint64

	logger Logger
}

func newUnstable(last entryID, logger Logger) unstable {
	// To initialize the last accepted term (logSlice.term) correctly, we make
	// sure its invariant is true: the log is a prefix of the term's leader's log.
	// This can be achieved by conservatively initializing to the term of the last
	// log entry.
	//
	// We can't pick any lower term because the lower term's leader can't have
	// last.term entries in it. We can't pick a higher term because we don't have
	// any information about the higher-term leaders and their logs. So last.term
	// is the only valid choice.
	//
	// TODO(pav-kv): persist the accepted term in HardState and load it. Our
	// initialization is conservative. Before restart, the accepted term could
	// have been higher. Setting a higher term (ideally, matching the current
	// leader Term) gives us more information about the log, and then allows
	// bumping its commit index sooner than when the next MsgApp arrives.
	return unstable{
		logSlice:   logSlice{term: last.term, prev: last},
		inProgress: last.index,
		logger:     logger,
	}
}

// maybeFirstIndex returns the index of the first possible entry in entries
// if it has a snapshot.
func (u *unstable) maybeFirstIndex() (uint64, bool) {
	if u.snapshot != nil {
		return u.snapshot.Metadata.Index + 1, true
	}
	return 0, false
}

// maybeTerm returns the term of the entry at index i, if there is any.
func (u *unstable) maybeTerm(i uint64) (uint64, bool) {
	if i < u.prev.index || i > u.lastIndex() {
		return 0, false
	}
	return u.termAt(i), true
}

// nextEntries returns the unstable entries that are not already in the process
// of being written to storage.
func (u *unstable) nextEntries() []pb.Entry {
	if u.inProgress == u.lastIndex() {
		return nil
	}
	return u.entries[u.inProgress-u.prev.index:]
}

// nextSnapshot returns the unstable snapshot, if one exists that is not already
// in the process of being written to storage.
func (u *unstable) nextSnapshot() *pb.Snapshot {
	if u.snapshot == nil || u.snapInProgress {
		return nil
	}
	return u.snapshot
}

// acceptInProgress marks all entries and the snapshot, if any, in the unstable
// as having begun the process of being written to storage. The entries/snapshot
// will no longer be returned from nextEntries/nextSnapshot. However, new
// entries/snapshots added after a call to acceptInProgress will be returned
// from those methods, until the next call to acceptInProgress.
func (u *unstable) acceptInProgress() {
	if u.snapshot != nil {
		u.snapInProgress = true
	}
	u.inProgress = u.lastIndex()
}

// stableTo marks entries up to the entry with the specified (index, term) as
// being successfully written to stable storage.
//
// The method should only be called when the caller can attest that the entries
// can not be overwritten by an in-progress log append. See the related comment
// in newStorageAppendRespMsg.
func (u *unstable) stableTo(mark logMark) {
	if mark.term < u.term {
		// Don't consider the appended log entries to be stable because they may
		// have been overwritten in the unstable log during a later term.
		u.logger.Infof("term changed from %d to %d; ignoring ack at index %d",
			mark.term, u.term, mark.index)
		return
	} else if mark.index < u.prev.index || mark.index > u.lastIndex() {
		u.logger.Infof("entry at index %d missing from unstable log; ignoring", mark.index)
		return
	} else if mark.index == u.prev.index {
		if u.snapshot != nil {
			u.logger.Infof("entry at index %d matched unstable snapshot; ignoring", mark.index)
		}
		return
	} else if u.snapshot != nil {
		u.logger.Panicf("entry %d acked earlier than the snapshot(in-progress=%t): %s",
			mark.index, u.snapInProgress, DescribeSnapshot(*u.snapshot))
		return
	}
	u.logSlice = u.forward(mark.index)
	u.inProgress = max(u.inProgress, mark.index)
	u.shrinkEntriesArray()
}

// shrinkEntriesArray discards the underlying array used by the entries slice
// if most of it isn't being used. This avoids holding references to a bunch of
// potentially large entries that aren't needed anymore. Simply clearing the
// entries wouldn't be safe because clients might still be using them.
func (u *unstable) shrinkEntriesArray() {
	// We replace the array if we're using less than half of the space in
	// it. This number is fairly arbitrary, chosen as an attempt to balance
	// memory usage vs number of allocations. It could probably be improved
	// with some focused tuning.
	const lenMultiple = 2
	if len(u.entries) == 0 {
		u.entries = nil
	} else if len(u.entries)*lenMultiple < cap(u.entries) {
		newEntries := make([]pb.Entry, len(u.entries))
		copy(newEntries, u.entries)
		u.entries = newEntries
	}
}

func (u *unstable) stableSnapTo(i uint64) {
	if u.snapshot != nil && u.snapshot.Metadata.Index == i {
		u.snapInProgress = false
		u.snapshot = nil
	}
}

func (u *unstable) restore(s snapshot) bool {
	// Do not allow regressing the state.
	if s.term <= u.term && s.lastIndex() < u.lastIndex() {
		// NB: today, this can't happen because the caller ensures s.term >= u.term,
		// and resorts to bumping commit index instead of applying the snapshot if
		// it matches the log (which will always happen when s.term == u.term).
		//
		// We err on the side of maintaining the append-only behaviour/invariant in
		// unstable structure, in case the caller behaviour changes. Not doing so
		// carries a risk of regressing the durable state and breaking raft.
		return false
	}
	// All logs at terms >= s.term are consistent with this snapshot. So if our
	// accepted term is > s.term, it is safe to retain it. This maintains the
	// invariant that accepted term never regresses.
	//
	// Today, the caller is conservative, and s.term >= u.term.
	accTerm := max(u.term, s.term)

	u.snapshot = &s.snap
	u.logSlice = logSlice{term: accTerm, prev: s.lastEntryID()}
	u.snapInProgress = false
	u.inProgress = u.prev.index
	return true
}

func (u *unstable) truncateAndAppend(a logSlice) bool {
	if a.term < u.term {
		return false // append from an outdated log
	}
	// Fast path for appends at the end of the log.
	last := u.lastEntryID()
	if a.prev == last {
		u.term = a.term // update the last accepted term
		u.entries = append(u.entries, a.entries...)
		return true
	}
	// If a.prev.index > last.index, we can not accept this write because it will
	// introduce a gap in the log.
	//
	// If a.prev.index == last.index, then the last entry term did not match in
	// the check above, so we must reject this case too.
	if a.prev.index >= last.index {
		return false
	}
	// Below, we handle the index regression case, a.prev.index < last.index.

	// The caller checks that a.prev.index >= commit, i.e. we are not truncating
	// committed entries. By extension, a.prev.index >= commit >= snapshot.index.
	// So we do not expect the following check to fail.
	//
	// It is a defense-in-depth guarding the invariant: if snapshot != nil then
	// prev == snapshot.{term,index}. The code below can regress prev, so we don't
	// want the snapshot ID to get out of sync with it.
	if u.snapshot != nil && a.prev.index < u.snapshot.Metadata.Index {
		u.logger.Panicf("appending entry %+v before snapshot %s",
			a.prev, DescribeSnapshot(*u.snapshot))
		return false
	}
	// Within the same leader term, we enforce the log to be append-only, and only
	// allow index regressions (which cause log truncations) when a.term > u.term.
	if a.term == u.term {
		return false
	}

	// Truncate the logSlice and append new entries. Regress the inProgress marker
	// to reflect that the truncated entries are no longer considered in progress.
	u.inProgress = min(u.inProgress, a.prev.index)
	if a.prev.index <= u.prev.index {
		u.logSlice = a // replace the entire logSlice with the latest append
		// TODO(pav-kv): clean up the logging message. It will change all datadriven
		// test outputs, so do it in a contained PR.
		u.logger.Infof("replace the unstable entries from index %d", a.prev.index+1)
	} else {
		u.term = a.term // update the last accepted term
		// Use the full slice expression to cause copy-on-write on this or a
		// subsequent (if a.entries is empty) append to u.entries. The truncated
		// part of the old slice can still be referenced elsewhere.
		keep := u.entries[:a.prev.index-u.prev.index]
		u.entries = append(keep[:len(keep):len(keep)], a.entries...)
		u.logger.Infof("truncate the unstable entries before index %d", a.prev.index+1)
	}
	return true
}

// slice returns the entries from the unstable log with indexes in the range
// [lo, hi). The entire range must be stored in the unstable log or the method
// will panic. The returned slice can be appended to, but the entries in it must
// not be changed because they are still shared with unstable.
//
// TODO(pavelkalinnikov): this, and similar []pb.Entry slices, may bubble up all
// the way to the application code through Ready struct. Protect other slices
// similarly, and document how the client can use them.
func (u *unstable) slice(lo uint64, hi uint64) []pb.Entry {
	u.mustCheckOutOfBounds(lo, hi)
	// NB: use the full slice expression to limit what the caller can do with the
	// returned slice. For example, an append will reallocate and copy this slice
	// instead of corrupting the neighbouring u.entries.
	offset := u.prev.index + 1
	return u.entries[lo-offset : hi-offset : hi-offset]
}

// u.offset <= lo <= hi <= u.offset+len(u.entries)
func (u *unstable) mustCheckOutOfBounds(lo, hi uint64) {
	if lo > hi {
		u.logger.Panicf("invalid unstable.slice %d > %d", lo, hi)
	}
	if last := u.lastIndex(); lo <= u.prev.index || hi > last+1 {
		u.logger.Panicf("unstable.slice[%d,%d) out of bound (%d,%d]", lo, hi, u.prev.index, last)
	}
}
