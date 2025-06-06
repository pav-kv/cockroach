// This code has been modified from its original form by The Cockroach Authors.
// All modifications are Copyright 2024 The Cockroach Authors.
//
// Copyright 2022 The etcd Authors
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

package rafttest

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/raft"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/datadriven"
)

func (env *InteractionEnv) handleProcessApplyThread(t *testing.T, d datadriven.TestData) error {
	idxs := nodeIdxs(t, d)
	for _, idx := range idxs {
		var err error
		if len(idxs) > 1 {
			fmt.Fprintf(env.Output, "> %d processing apply thread\n", idx+1)
			env.withIndent(func() { err = env.ProcessApplyThread(idx) })
		} else {
			err = env.ProcessApplyThread(idx)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ProcessApplyThread runs processes a single message on the "apply" thread of
// the node with the given index.
func (env *InteractionEnv) ProcessApplyThread(idx int) error {
	n := &env.Nodes[idx]
	if n.ApplyWork.Empty() {
		env.Output.WriteString("no apply work to perform")
		return nil
	}

	const limit = 1 // TODO(pav-kv): make it configurable.
	work := n.ApplyWork
	work.Last = work.After + limit

	ls := n.RawNode.LogSnapshot()
	apply, err := ls.Slice(work, n.Config.MaxCommittedSizePerReady)
	if err != nil {
		return err
	} else if len(apply) == 0 {
		return fmt.Errorf("no entries loaded for apply span %v", work)
	}

	env.Output.WriteString("Applying:\n")
	env.Output.WriteString(raft.DescribeEntries(apply, defaultEntryFormatter))
	if err := processApply(n, apply); err != nil {
		return err
	}
	n.ApplyWork.After = raftpb.Index(apply[len(apply)-1].Index)
	n.AckApplied(apply)
	return nil
}

func processApply(n *Node, ents []raftpb.Entry) error {
	for _, ent := range ents {
		var update []byte
		var cs *raftpb.ConfState
		switch ent.Type {
		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			if err := cc.Unmarshal(ent.Data); err != nil {
				return err
			}
			update = cc.Context
			cs = n.RawNode.ApplyConfChange(cc)
		case raftpb.EntryConfChangeV2:
			var cc raftpb.ConfChangeV2
			if err := cc.Unmarshal(ent.Data); err != nil {
				return err
			}
			cs = n.RawNode.ApplyConfChange(cc)
			update = cc.Context
		default:
			update = ent.Data
		}

		// Record the new state by starting with the current state and applying
		// the command.
		lastSnap := n.History[len(n.History)-1]
		var snap raftpb.Snapshot
		snap.Data = append(snap.Data, lastSnap.Data...)
		// NB: this hard-codes an "appender" state machine.
		snap.Data = append(snap.Data, update...)
		snap.Metadata.Index = ent.Index
		snap.Metadata.Term = ent.Term
		if cs == nil {
			sl := n.History
			cs = &sl[len(sl)-1].Metadata.ConfState
		}
		snap.Metadata.ConfState = *cs
		n.History = append(n.History, snap)
	}
	return nil
}
