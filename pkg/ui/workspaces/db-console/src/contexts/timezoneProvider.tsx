// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import { TimezoneContext } from "@cockroachlabs/cluster-ui";
import React, { useEffect } from "react";
import { useDispatch, useSelector } from "react-redux";

import { refreshSettings } from "src/redux/apiReducers";
import {
  selectClusterSettings,
  selectTimezoneSetting,
} from "src/redux/clusterSettings";

export const TimezoneProvider = (props: any) => {
  // Refresh cluster settings if needed.
  const dispatch = useDispatch();
  const settings = useSelector(selectClusterSettings);
  useEffect(() => {
    dispatch(refreshSettings());
  }, [settings, dispatch]);

  // Grab the timezone value from the store, and pass it to our context.
  const timezone = useSelector(selectTimezoneSetting);
  return (
    <TimezoneContext.Provider value={timezone}>
      {props.children}
    </TimezoneContext.Provider>
  );
};
