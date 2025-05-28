// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserverpb

import (
	"context"
	"strconv"

	"github.com/cockroachdb/errors"
)

// TODOLogID is the zero LogID which identifies the old way of storing raft
// state under RangeID-local keys. The new way will place it under LogID-aware
// keys.
// TODO(sep-raft-log): make all users of this const aware of the new schema.
const TODOLogID = LogID(0)

// TODOLogIDRotate is a placeholder for the code which should increment LogID
// and place the raft state under the new LogID. This is done when creating an
// uninitialized replica, or when an initialized replica applies a snapshot.
const TODOLogIDRotate = LogID(0)

// LogID is a unique ID for a RangeID's raft state in local Store.
type LogID uint64

// String implements the fmt.Stringer interface.
func (id LogID) String() string {
	return strconv.FormatUint(uint64(id), 10)
}

// SafeValue implements the redact.SafeValue interface.
func (id LogID) SafeValue() {}

// Error returns the error contained in the snapshot response, if any.
//
// The bool indicates whether this message uses the deprecated behavior of
// encoding an error as a string.
func (m *DelegateSnapshotResponse) Error() error {
	if m.Status != DelegateSnapshotResponse_ERROR {
		return nil
	}
	return errors.DecodeError(context.Background(), m.EncodedError)
}

// Error returns the error contained in the snapshot response, if any.
//
// The bool indicates whether this message uses the deprecated behavior of
// encoding an error as a string.
func (m *SnapshotResponse) Error() (deprecated bool, _ error) {
	if m.Status != SnapshotResponse_ERROR {
		return false, nil
	}
	if m.EncodedError.IsSet() {
		return false, errors.DecodeError(context.Background(), m.EncodedError)
	}
	return true, errors.Newf("%s", m.DeprecatedMessage)
}
