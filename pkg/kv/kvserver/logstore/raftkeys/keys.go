package raftkeys

import (
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
)

// KeyBuf provides methods for generating storage engine keys for raft state.
// The keys are generated in the in-place buffer, and are only valid until the
// next call to one of the methods.
type KeyBuf roachpb.Key

// MakeKeyBuf creates a KeyBuf set to generate raft state keys within the
// RangeID-local unreplicated keyspace.
func MakeKeyBuf(rangeID roachpb.RangeID, logID kvpb.LogID) KeyBuf {
	if logID == 0 {
		return KeyBuf(keys.MakeRangeIDUnreplicatedPrefix(rangeID))
	}
	return KeyBuf(keys.MakeRangeIDLogIDPrefix(rangeID, logID))
}

// RaftHardStateKey returns the key for raft HardState.
func (b KeyBuf) RaftHardStateKey() roachpb.Key {
	return roachpb.Key(append(b, keys.LocalRaftHardStateSuffix...))
}

// RaftTruncatedStateKey returns the key for RaftTruncatedState.
func (b KeyBuf) RaftTruncatedStateKey() roachpb.Key {
	return roachpb.Key(append(b, keys.LocalRaftTruncatedStateSuffix...))
}

// RaftLogPrefix returns the key prefix shared by all entries in the raft log.
func (b KeyBuf) RaftLogPrefix() roachpb.Key {
	return roachpb.Key(append(b, keys.LocalRaftLogSuffix...))
}

// RaftLogKey returns the key for a raft log entry.
func (b KeyBuf) RaftLogKey(index kvpb.RaftIndex) roachpb.Key {
	return keys.RaftLogKeyFromPrefix(b.RaftLogPrefix(), index)
}
