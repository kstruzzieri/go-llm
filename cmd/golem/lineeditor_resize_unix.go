//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// watchResize forwards SIGWINCH as terminal-resize notifications and returns an
// idempotent stop.
//
// The forwarding goroutine selects on both the signal channel and a private
// done channel, and stop closes done rather than the channel os/signal owns.
// Delivery coalesces into a capacity-one channel with a nonblocking send, so the
// forwarder can never block merely because nothing is currently consuming
// resizes -- which is exactly the state Close finds it in.
func watchResize() (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)

	resize := make(chan struct{}, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case <-sig:
				select {
				case resize <- struct{}{}:
				default: // one pending notification already says "size changed"
				}
			}
		}
	}()

	var once sync.Once
	return resize, func() {
		once.Do(func() {
			signal.Stop(sig)
			close(done)
			wg.Wait()
		})
	}
}
