// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import { all, fork } from "redux-saga/effects";

import { timeScaleSaga } from "src/redux/timeScale";

import { analyticsSaga } from "./analyticsSagas";
import { customAnalyticsSaga } from "./customAnalytics";
import { indexUsageStatsSaga } from "./indexUsageStats";
import { jobsSaga } from "./jobs/jobsSagas";
import { localSettingsSaga } from "./localsettings";
import { queryMetricsSaga } from "./metrics";
import { sessionsSaga } from "./sessions";
import { sqlStatsSaga } from "./sqlStats";
import { statementsSaga } from "./statements";

export default function* rootSaga() {
  yield all([
    fork(queryMetricsSaga),
    fork(localSettingsSaga),
    fork(customAnalyticsSaga),
    fork(statementsSaga),
    fork(jobsSaga),
    fork(analyticsSaga),
    fork(sessionsSaga),
    fork(sqlStatsSaga),
    fork(indexUsageStatsSaga),
    fork(timeScaleSaga),
  ]);
}
