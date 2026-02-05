// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mpsc

import (
	"slices"
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

func TestQueueWorkload(t *testing.T) {
	q := NewQueue[uint64]()

	const workers = 10
	const values = 1000

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

	got := make([]uint64, 0, workers*values)

	for ack := uint64(0); len(got) < cap(got); {
		next := q.get(ack)
		got = append(got, next...)
		ack = uint64(len(next))
	}
	q.close()

	slices.Sort(got)
	for i, x := range got {
		require.Equal(t, uint64(i), x)
	}
}
