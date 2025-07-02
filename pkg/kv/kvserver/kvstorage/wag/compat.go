// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package wag

import (
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
)

var Enabled = envutil.EnvOrDefaultBool("COCKROACH_ENABLE_WAG", false)
var LogIDEnabled = envutil.EnvOrDefaultBool("COCKROACH_ENABLE_LOGID", false)

func NextLogID(next kvpb.LogID) kvpb.LogID {
	if LogIDEnabled {
		return next
	}
	return kvpb.TODOLogID
}
