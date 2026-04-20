package compat

import (
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestReserveFor(t *testing.T) {
	cases := []struct {
		in                   int
		wantGen, wantReserve int
	}{
		{0, 1, 0},
		{1, 1, 0},
		{2, 2, 0},
		{3, 3, 0},
		{4, 3, 1},
		{8, 7, 1},
		{100, 99, 1},
	}
	for _, tc := range cases {
		gen, res := reserveFor(tc.in)
		if gen != tc.wantGen || res != tc.wantReserve {
			t.Errorf("reserveFor(%d) = (%d, %d), want (%d, %d)",
				tc.in, gen, res, tc.wantGen, tc.wantReserve)
		}
	}
}

func TestSemaphore_AcquireRelease(t *testing.T) {
	sem := newSemaphore(2)
	r1, ok := sem.acquire(provider.PriorityNormal)
	if !ok {
		t.Fatal("acquire 1 failed")
	}
	r2, ok := sem.acquire(provider.PriorityNormal)
	if !ok {
		t.Fatal("acquire 2 failed")
	}
	if _, ok := sem.acquire(provider.PriorityNormal); ok {
		t.Fatal("acquire 3 must fail (cap=2)")
	}
	r1()
	if _, ok := sem.acquire(provider.PriorityNormal); !ok {
		t.Fatal("acquire after release must succeed")
	}
	r2()
}

func TestSemaphore_HighPriorityUsesReserve(t *testing.T) {
	// cap=4 -> general=3, reserve=1.
	sem := newSemaphore(4)

	// Fill the general pool with normal requests.
	releases := make([]func(), 0, 3)
	for i := 0; i < 3; i++ {
		r, ok := sem.acquire(provider.PriorityNormal)
		if !ok {
			t.Fatalf("acquire normal %d failed", i)
		}
		releases = append(releases, r)
	}
	// Normal request blocks.
	if _, ok := sem.acquire(provider.PriorityNormal); ok {
		t.Fatal("normal must be blocked once general pool is full")
	}
	// High-priority still fits in the reserve.
	rHigh, ok := sem.acquire(provider.PriorityHigh)
	if !ok {
		t.Fatal("PriorityHigh must use reserve lane")
	}
	// Now every slot is taken.
	if _, ok := sem.acquire(provider.PriorityHigh); ok {
		t.Fatal("second high must be blocked (reserve=1)")
	}
	rHigh()
	for _, r := range releases {
		r()
	}
}

func TestSemaphore_HighPriorityFallsBackToGeneralWhenReserveZero(t *testing.T) {
	// cap=2 -> general=2, reserve=0.
	sem := newSemaphore(2)
	r1, ok := sem.acquire(provider.PriorityHigh)
	if !ok {
		t.Fatal("high must acquire general slot when reserve=0")
	}
	r2, ok := sem.acquire(provider.PriorityHigh)
	if !ok {
		t.Fatal("high must acquire second general slot when reserve=0")
	}
	if _, ok := sem.acquire(provider.PriorityHigh); ok {
		t.Fatal("third high must fail (cap=2)")
	}
	r1()
	r2()
}

func TestSemaphore_Concurrent(t *testing.T) {
	sem := newSemaphore(4)
	var wg sync.WaitGroup
	var acquired int
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := sem.acquire(provider.PriorityNormal); ok {
				mu.Lock()
				acquired++
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				release()
			}
		}()
	}
	wg.Wait()
	if acquired == 0 {
		t.Fatal("no acquisitions succeeded")
	}
}
