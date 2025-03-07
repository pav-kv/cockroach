// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver

import (
	"context"
	"math/rand"
	"runtime"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/benignerror"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/spanconfig"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
)

func constantTimeoutFunc(d time.Duration) func(*cluster.Settings, replicaInQueue) time.Duration {
	return func(*cluster.Settings, replicaInQueue) time.Duration { return d }
}

// TestBaseQueueConcurrent verifies that under concurrent adds/removes of ranges
// to the queue including purgatory errors and regular errors, the queue
// invariants are upheld. The test operates on fake ranges and a mock queue
// impl, which are defined at the end of the file.
func TestBaseQueueConcurrent(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	tr := tracing.NewTracer()
	stopper := stop.NewStopper(stop.WithTracer(tr))
	defer stopper.Stop(ctx)

	// We'll use this many ranges, each of which is added a few times to the
	// queue and maybe removed as well.
	const num = 1000

	cfg := queueConfig{
		maxSize:              num / 2,
		maxConcurrency:       4,
		acceptsUnsplitRanges: true,
		processTimeoutFunc:   constantTimeoutFunc(time.Millisecond),
		// We don't care about these, but we don't want to crash.
		successes:       metric.NewCounter(metric.Metadata{Name: "processed"}),
		failures:        metric.NewCounter(metric.Metadata{Name: "failures"}),
		pending:         metric.NewGauge(metric.Metadata{Name: "pending"}),
		processingNanos: metric.NewCounter(metric.Metadata{Name: "processingnanos"}),
		purgatory:       metric.NewGauge(metric.Metadata{Name: "purgatory"}),
		disabledConfig:  testQueueEnabled,
	}

	// Set up a fake store with just exactly what the code calls into. Ideally
	// we'd set up an interface against the *Store as well, similar to
	// replicaInQueue, but this isn't an ideal world. Deal with it.
	store := &Store{
		cfg: StoreConfig{
			Clock:             hlc.NewClockForTesting(nil),
			AmbientCtx:        log.MakeTestingAmbientContext(tr),
			DefaultSpanConfig: roachpb.TestingDefaultSpanConfig(),
			Settings:          cluster.MakeTestingClusterSettingsWithVersions(clusterversion.Latest.Version(), clusterversion.Latest.Version(), true),
		},
	}

	// Set up a queue impl that will return random results from processing.
	impl := fakeQueueImpl{
		pr: func(context.Context, *Replica, spanconfig.StoreReader) (bool, error) {
			n := rand.Intn(4)
			if n == 0 {
				return true, nil
			} else if n == 1 {
				return false, errors.New("injected regular error")
			} else if n == 2 {
				return false, benignerror.New(errors.New("injected benign error"))
			}
			return false, &testPurgatoryError{}
		},
	}
	bq := newBaseQueue("test", impl, store, cfg)
	bq.getReplica = func(id roachpb.RangeID) (replicaInQueue, error) {
		return &fakeReplica{rangeID: id}, nil
	}
	bq.Start(stopper)

	var g errgroup.Group
	for i := 1; i <= num; i++ {
		r := &fakeReplica{rangeID: roachpb.RangeID(i)}
		for j := 0; j < 5; j++ {
			g.Go(func() error {
				_, err := bq.testingAdd(ctx, r, 1.0)
				return err
			})
		}
		if rand.Intn(5) == 0 {
			g.Go(func() error {
				bq.MaybeRemove(r.rangeID)
				return nil
			})
		}
		g.Go(func() error {
			bq.assertInvariants()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	for done := false; !done; {
		bq.mu.Lock()
		done = len(bq.mu.replicas) == 0
		bq.mu.Unlock()
		runtime.Gosched()
	}
}

type fakeQueueImpl struct {
	pr func(context.Context, *Replica, spanconfig.StoreReader) (processed bool, err error)
}

var _ queueImpl = &fakeQueueImpl{}

func (fakeQueueImpl) shouldQueue(
	context.Context, hlc.ClockTimestamp, *Replica, spanconfig.StoreReader,
) (shouldQueue bool, priority float64) {
	return rand.Intn(5) != 0, 1.0
}

func (fq fakeQueueImpl) process(
	ctx context.Context, repl *Replica, confReader spanconfig.StoreReader,
) (bool, error) {
	return fq.pr(ctx, repl, confReader)
}

func (fakeQueueImpl) postProcessScheduled(
	ctx context.Context, replica replicaInQueue, priority float64,
) {
}

func (fakeQueueImpl) timer(time.Duration) time.Duration {
	return time.Nanosecond
}

func (fakeQueueImpl) purgatoryChan() <-chan time.Time {
	return time.After(time.Nanosecond)
}

func (fakeQueueImpl) updateChan() <-chan time.Time {
	return nil
}

type fakeReplica struct {
	rangeID   roachpb.RangeID
	replicaID roachpb.ReplicaID
}

func (fr *fakeReplica) AnnotateCtx(ctx context.Context) context.Context { return ctx }
func (fr *fakeReplica) StoreID() roachpb.StoreID {
	return 1
}
func (fr *fakeReplica) GetRangeID() roachpb.RangeID         { return fr.rangeID }
func (fr *fakeReplica) ReplicaID() roachpb.ReplicaID        { return fr.replicaID }
func (fr *fakeReplica) IsInitialized() bool                 { return true }
func (fr *fakeReplica) IsDestroyed() (DestroyReason, error) { return destroyReasonAlive, nil }
func (fr *fakeReplica) Desc() *roachpb.RangeDescriptor {
	return &roachpb.RangeDescriptor{RangeID: fr.rangeID, EndKey: roachpb.RKey("z")}
}
func (fr *fakeReplica) redirectOnOrAcquireLease(
	context.Context,
) (kvserverpb.LeaseStatus, *kvpb.Error) {
	// baseQueue only checks that the returned error is nil.
	return kvserverpb.LeaseStatus{}, nil
}
func (fr *fakeReplica) CurrentLeaseStatus(context.Context) kvserverpb.LeaseStatus {
	return kvserverpb.LeaseStatus{}
}
