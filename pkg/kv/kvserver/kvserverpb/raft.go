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
