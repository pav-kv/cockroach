// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package gpq

import (
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/errors"
)

var CheckClusterSupportsGenericQueryPlans = func(settings *cluster.Settings) error {
	return sqlerrors.NewCCLRequiredError(
		errors.New("plan_cache_mode=force_generic_plan and plan_cache_mode=auto require a CCL binary"),
	)
}
