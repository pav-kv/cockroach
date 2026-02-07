package kvserver

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/mpsc"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/crlib/crtime"
)

type workerID int

type rangeIDState struct {
	rangeID roachpb.RangeID
	flags   atomic.Uint64 // raftScheduleFlags
	ticks   atomic.Uint64 // updated before flags, but the other way when dequeued

	// The fields below are written just before the rangeIDState is enqueued, and
	// read by the worker when it dequeues before resetting the flags.
	queued crtime.Mono // last time this rangeID was enqueued
	worker workerID    // last worker this rangeID was enqueued to
}

func makeRangeIDState(id roachpb.RangeID) rangeIDState {
	return rangeIDState{rangeID: id, worker: -1}
}

func (r *rangeIDState) set(flag raftScheduleFlags) (enqueue bool) {
	if flag == stateRaftTick {
		r.ticks.Add(1)
	}
	return r.flags.Or(uint64(stateQueued|flag))&uint64(stateQueued) == 0
}

func (r *rangeIDState) clear() (_ raftScheduleFlags, ticks uint64) {
	flags := raftScheduleFlags(r.flags.Swap(0))
	if flags&stateRaftTick == 0 {
		return flags, 0
	}
	return flags, r.ticks.Swap(0)
}

type scheduler struct {
	ambientCtx log.AmbientContext
	processor  raftProcessor
	metrics    *StoreMetrics

	m syncutil.Map[roachpb.RangeID, rangeIDState]
	w []*schedWorker

	done sync.WaitGroup
}

func newScheduler(
	ambient log.AmbientContext, processor raftProcessor, metrics *StoreMetrics, numWorkers int,
) *scheduler {
	w := make([]*schedWorker, numWorkers)
	for i := range w {
		w[i] = newSchedWorker(processor, metrics)
	}
	return &scheduler{
		ambientCtx: ambient,
		processor:  processor,
		metrics:    metrics,
		w:          w,
	}
}

func (s *scheduler) Start(stopper *stop.Stopper) {
	stopper.OnQuiesce(func() {
		for _, w := range s.w {
			w.q.Close()
		}
	})

	ctx := s.ambientCtx.AnnotateCtx(context.Background())
	for _, w := range s.w {
		ctx, hdl, err := stopper.GetHandle(ctx, stop.TaskOpts{
			TaskName: "raft-worker",
			// Doesn't reference a parent because it runs for the server's lifetime.
			SpanOpt: stop.SterileRootSpan,
		})
		if err != nil {
			continue
		}
		s.done.Go(func() {
			defer hdl.Activate(ctx).Release(ctx)
			w.run(ctx)
		})
	}
}

func (s *scheduler) get(id roachpb.RangeID) *rangeIDState {
	state, ok := s.m.Load(id)
	if !ok {
		init := makeRangeIDState(id)
		state, _ = s.m.LoadOrStore(id, &init)
	}
	return state
}

func (s *scheduler) enqueue1(state *rangeIDState, flag raftScheduleFlags) {
	// Set the dirty bit, and return early if the range ID is already enqueued.
	if !state.set(flag) {
		return
	}
	// Otherwise, we are the first to set the stateQueued bit, and have the
	// exclusive responsibility of enqueueing the range ID to a worker.
	// Pick a worker, and remember it to be sticky.
	worker := state.worker
	if worker == -1 {
		worker = workerID(shardIndex(state.rangeID, len(s.w), false /* priority*/))
		state.worker = worker
	}
	// Enqueue the range ID to the worker.
	state.queued = crtime.NowMono()
	s.w[worker].enqueue(state)
}

type schedWorker struct {
	q *mpsc.Queue[*rangeIDState]
	p raftProcessor
	m *StoreMetrics
}

func newSchedWorker(p raftProcessor, m *StoreMetrics) *schedWorker {
	return &schedWorker{q: mpsc.NewQueue[*rangeIDState](1024), p: p, m: m}
}

func (w *schedWorker) enqueue(state *rangeIDState) {
	w.q.Put(state)
}

func (w *schedWorker) run(ctx context.Context) {
	for states := w.q.Get(0); len(states) > 0; states = w.q.Get(uint64(len(states))) {
		for _, state := range states {
			w.process(ctx, state)
		}
		clear(states)
	}
}

func (w *schedWorker) process(ctx context.Context, state *rangeIDState) {
	// Record the scheduling latency for the range.
	rangeID := state.rangeID
	w.m.RaftSchedulerLatency.RecordValue(int64(state.queued.Elapsed()))
	// Atomically dequeue all the dirty bits.
	flags, ticks := state.clear()

	// Process requests first. This avoids a scenario where a tick and a
	// "quiesce" message are processed in the same iteration and intervening
	// raft ready processing unquiesces the replica because the tick triggers
	// an election.
	if flags&stateRaftRequest != 0 {
		// processRequestQueue returns true if the range should perform ready
		// processing. Do not reorder this below the call to processReady.
		if w.p.processRequestQueue(ctx, rangeID) {
			flags |= stateRaftReady
		}
	}
	if util.RaceEnabled { // assert the ticks invariant
		if tick := flags&stateRaftTick != 0; tick != (ticks != 0) {
			log.KvExec.Fatalf(ctx, "stateRaftTick is %v with ticks %v", tick, ticks)
		}
	}
	if flags&stateRaftTick != 0 {
		for t := ticks; t > 0; t-- {
			// processRaftTick returns true if the range should perform ready
			// processing. Do not reorder this below the call to processReady.
			if w.p.processTick(ctx, rangeID) {
				flags |= stateRaftReady
			}
		}
	}
	if flags&stateRACv2PiggybackedAdmitted != 0 {
		w.p.processRACv2PiggybackedAdmitted(ctx, rangeID)
	}
	if flags&stateRaftReady != 0 {
		w.p.processReady(rangeID)
	}
	if flags&stateRACv2RangeController != 0 {
		w.p.processRACv2RangeController(ctx, rangeID)
	}
	if buildutil.CrdbTestBuild && flags&stateTestIntercept != 0 {
		// w.p.(testProcessorI).processTestEvent(q, ss, state)
	}
}
