// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mpsc

import (
	"math"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
)

type Queue[T any] struct {
	head atomic.Pointer[chunk[T]]
	tail *chunk[T]
}

func NewQueue[T any]() *Queue[T] {
	q := &Queue[T]{tail: newChunk[T](16)}
	q.head.Store(q.tail)
	return q
}

func (q *Queue[T]) get(ack uint64) []T {
	if got := q.tail.get(ack); len(got) > 0 {
		return got
	}
	for {
		q.tail = q.tail.waitNext()
		if got := q.tail.get(0); len(got) > 0 {
			return got
		}
	}
}

func (q *Queue[T]) put(value T) bool {
	head := q.head.Load()
	for ok, doClose := head.put(value); !ok; ok, doClose = head.put(value) {
		if doClose {
			next := newChunk[T](sizePolicy(head.lenMask + 1)) // TODO: sync.Pool
			q.head.Store(next)
			head.close(next)
			head = next
		} else if head = head.waitNext(); head == nil {
			return false
		}
	}
	return true
}

func (q *Queue[T]) close() {
	q.head.Load().close(nil)
}

const chunkIndexEnd = uint64(math.MaxUint64)>>3 + 1

type chunk[T any] struct {
	mu    syncutil.RWMutex
	ready sync.Cond

	buf     []T
	lenMask uint64 // len(buf) - 1 == 2^p - 1

	read  uint64
	write uint64
	end   uint64
	next  *chunk[T]

	done chan struct{}
}

func newChunk[T any](size uint64) *chunk[T] {
	if buildutil.CrdbTestBuild && size&(size-1) != 0 {
		panic("size must be a power of 2")
	}
	c := &chunk[T]{
		buf:     make([]T, size),
		lenMask: size - 1,
		end:     chunkIndexEnd,
		done:    make(chan struct{}),
	}
	c.ready.L = &c.mu
	return c
}

func (c *chunk[T]) get(ack uint64) []T {
	begin, end := c.ackAndWait(ack)
	if begin >= end {
		return nil
	}
	// Map the [begin, end) span onto the circular buffer.
	begin &= c.lenMask
	end &= c.lenMask
	// If the readable slice does not wrap around, return it in full.
	if begin < end {
		return c.buf[begin:end]
	}
	// if the readable slice wraps around, only return a contiguous prefix. The
	// consumer will come back for the suffix later.
	return c.buf[begin:]
}

func (c *chunk[T]) ackAndWait(ack uint64) (begin, end uint64) {
	// NB: write lock, to wait until all the concurrent puts are flushed.
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read += ack
	for c.read >= c.write && c.read < c.end {
		c.ready.Wait()
	}
	return c.read, min(c.write, c.end)
}

func (c *chunk[T]) put(value T) (ok, doClose bool) {
	// Use the read lock to allow concurrent inserts.
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.next != nil {
		return false, false
	}

	// TODO(pav-kv): panic when it grows too large, at the risk of overflowing
	// back to 0. Below, we close the chunk way earlier, and in practice the
	// overflow can't happen. But still.
	index := atomic.AddUint64(&c.write, 1) - 1 // next write index

	if end := min(chunkIndexEnd, c.read+c.lenMask+1); index >= end {
		// We ran out of chunk space or indices. Close the chunk. The exclusive
		// privilege of updating the end index lies with the first caller (among
		// ones holding the read lock) who noticed the overflow. It must also
		// subsequently call the close() method.
		if index != end {
			return false, false
		}
		c.end = end
		c.next = c // hack
		return false, true
	}

	// Store the value in the circular buffer slot.
	c.buf[index&c.lenMask] = value
	// Wake up the consumer in case it is waiting for the first item to appear.
	if index == c.read {
		c.ready.Signal()
	}
	return true, false
}

func (c *chunk[T]) multiPut(_ []T) {
	// zero copy? give the caller a slice to populate, while holding RLock.
	panic("implement me")
}

func (c *chunk[T]) waitNext() *chunk[T] {
	<-c.done
	return c.next
}

func (c *chunk[T]) close(next *chunk[T]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next = next
	c.end = min(c.end, c.write)
	close(c.done)
}

// heuristics:
// - grow the chunk 2x every time the previous one fills up
// - when to shrink it down?
func sizePolicy(prevSize uint64) uint64 {
	// TODO(pav-kv): also support shrinking when the queue load is low.
	const maxSize = 1 << 17 // ~131k
	return min(prevSize*2, maxSize)
}
