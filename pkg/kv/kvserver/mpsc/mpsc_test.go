// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mpsc

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueue(t *testing.T) {
	q := NewQueue[uint64]()
	q.put(100)
	q.get(0)
	require.Equal(t, []uint64{100}, q.get(0))
	q.put(200)
	q.put(300)
	q.get(1)
	require.Equal(t, []uint64{200, 300}, q.get(0))
	q.close()
}

func BenchmarkQueue(b *testing.B) {
	const values = 100000000
	for _, w := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("%dx", w), func(b *testing.B) {
			for b.Loop() {
				benchOnce(w, values/w)
			}
		})
	}
}

func benchOnce(workers, values int) {
	q := NewQueue[uint64]()

	var wg sync.WaitGroup
	defer wg.Wait()
	for w := range workers {
		begin, end := w*values, (w+1)*values
		wg.Go(func() {
			for i := begin; i < end; i++ {
				q.put(uint64(i))
			}
		})
	}

	var got int
	for ack, mx := 0, workers*values; got < mx; {
		ack = len(q.get(uint64(ack)))
		got += ack
	}
	q.close()
}

func BenchmarkChan(b *testing.B) {
	const values = 100000000
	for _, w := range []int{1, 2, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("%dx", w), func(b *testing.B) {
			for b.Loop() {
				benchOnce(w, values/w)
			}
		})
	}
}

func benchOnceChan(workers, values int) {
	q := make(chan uint64, 100000)

	var wg sync.WaitGroup
	defer wg.Wait()
	for w := range workers {
		begin, end := w*values, (w+1)*values
		wg.Go(func() {
			for i := begin; i < end; i++ {
				q <- uint64(i)
			}
		})
	}

	var got int
	for mx := workers * values; got < mx; {
		x := <-q
		_ = x
		got++
	}
	close(q)
}
