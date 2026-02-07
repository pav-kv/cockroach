// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mpsc

import (
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/crlib/crtime"
	"github.com/stretchr/testify/require"
)

func TestQueue(t *testing.T) {
	q := NewQueue[uint64](16)
	q.Put(100)
	q.Get(0)
	require.Equal(t, []uint64{100}, q.Get(0))
	q.Put(200)
	q.Put(300)
	q.Get(1)
	require.Equal(t, []uint64{200, 300}, q.Get(0))
	q.Close()
}

func BenchmarkQueue(b *testing.B) {
	const values = 100000000
	for _, w := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("%dx", w), func(b *testing.B) {
			for b.Loop() {
				benchOnce(b, w, values/w, false)
			}
		})
	}
}

func BenchmarkQueueFast(b *testing.B) {
	const values = 100000000
	for _, w := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("%dx", w), func(b *testing.B) {
			for b.Loop() {
				benchOnce(b, w, values/w, true)
			}
		})
	}
}

func benchOnce(b *testing.B, workers, values int, wait bool) {
	b.StopTimer()
	q := NewQueue[crtime.Mono](1 << 22)
	lat := make([]time.Duration, 0, workers*values)

	start := make(chan struct{})
	var wg sync.WaitGroup

	for w := range workers {
		begin, end := w*values, (w+1)*values
		wg.Go(func() {
			<-start
			for i := begin; i < end; i++ {
				q.Put(crtime.NowMono())
			}
		})
	}
	b.StartTimer()
	close(start)

	if wait {
		wg.Wait()
	} else {
		defer wg.Wait()
	}

	var got int
	for ack, mx := 0, workers*values; got < mx; {
		vals := q.Get(uint64(ack))
		now := crtime.NowMono()
		for _, v := range vals {
			lat = append(lat, now.Sub(v))
		}
		ack = len(vals)
		got += ack
		runtime.Gosched()
	}
	q.Close()
	b.StopTimer()

	slices.Sort(lat)
	fmt.Println(lat[0], lat[len(lat)-1])
	var sum float64
	for _, l := range lat {
		sum += float64(l.Nanoseconds())
	}
	b.ReportMetric(float64(lat[len(lat)-1])/1e3, "p100-latency(us)")
	b.ReportMetric(float64(lat[0])/1e3, "p0-latency(us)")
	b.ReportMetric(sum/float64(len(lat))/1e3, "avg-latency(us)")
	for _, p := range []float64{50, 99, 99.9, 99.99} {
		b.ReportMetric(float64(lat[int(float64(len(lat))*p/100)])/1e3, fmt.Sprintf("p%.2f-latency(us)", p))
	}

	b.StartTimer()
}

func BenchmarkChan(b *testing.B) {
	const values = 100000000
	for _, w := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("%dx", w), func(b *testing.B) {
			for b.Loop() {
				benchOnceChan(b, w, values/w)
			}
		})
	}
}

func benchOnceChan(b *testing.B, workers, values int) {
	b.StopTimer()
	q := make(chan uint64, 1<<22)
	b.StartTimer()

	start := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()

	for w := range workers {
		begin, end := w*values, (w+1)*values
		wg.Go(func() {
			<-start
			for i := begin; i < end; i++ {
				q <- uint64(i)
			}
		})
	}
	b.StartTimer()
	close(start)

	var got int
	for mx := workers * values; got < mx; {
		x := <-q
		_ = x
		got++
	}
	close(q)
}
