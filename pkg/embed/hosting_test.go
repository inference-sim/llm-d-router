/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package embed

import (
	"context"
	"sync"
	"testing"
	"time"

	contract "github.com/inference-sim/blis-schemas/embed"
	"github.com/inference-sim/blis-schemas/embed/embedtest"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
)

// TestStopResumesChildrenFirst: a router loop that waits for a goroutine it started without
// parking (as the flow-control processor waits on its cleanup sweep's WaitGroup) is resumed at
// Stop only after that goroutine has ended, so a host that resumes parked goroutines oldest first
// does not deadlock. Without the adapter's order, the parent blocks in Wait holding the baton and
// the child never runs: this test then hangs until go test's timeout.
func TestStopResumesChildrenFirst(t *testing.T) {
	host := embedtest.NewRuntime(1)
	hrt, stopping := adaptRuntime(host)
	ctx, cancel := context.WithCancel(hosting.Into(context.Background(), hrt))
	var order []string
	tick := 0 // a wake-up both loops observe before Stop, so the host re-sorts them by age
	loop := func() {
		for seen := 0; ; {
			hrt.Park(func() bool { return ctx.Err() != nil || tick > seen })
			if ctx.Err() != nil {
				return
			}
			seen = tick
		}
	}
	hrt.Go(func() { // the parent, started first: resumed first by an oldest-first host
		var wg sync.WaitGroup
		wg.Add(1)
		hrt.Go(func() {
			defer wg.Done()
			loop()
			order = append(order, "child")
		})
		loop()
		wg.Wait() // not parked: the child must already have ended
		order = append(order, "parent")
	})
	tick++
	host.Settle()
	stopping()
	cancel()
	done := make(chan struct{})
	go func() { host.Settle(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second): //blis:wallclock-ok a test's hang guard
		t.Fatal("Stop deadlocked: the parent was resumed before its child ended")
	}
	if len(order) != 2 || order[0] != "child" || order[1] != "parent" {
		t.Fatalf("shutdown order %v, want [child parent]", order)
	}
	if n := host.Live(); n != 0 {
		t.Errorf("%d goroutines live after Stop", n)
	}
}

// TestStopDoesNotHoldAnEarlierGoroutineForALaterOneThatWaitsOnIt: a later goroutine that, at
// Stop, can only proceed once an earlier one has run does not hold the earlier one back.
func TestStopDoesNotHoldAnEarlierGoroutineForALaterOneThatWaitsOnIt(t *testing.T) {
	host := embedtest.NewRuntime(1)
	hrt, stopping := adaptRuntime(host)
	ctx, cancel := context.WithCancel(hosting.Into(context.Background(), hrt))
	released := false
	var order []string
	hrt.Go(func() { // earlier: ends at Stop and releases the later one
		hrt.Park(func() bool { return ctx.Err() != nil })
		released = true
		order = append(order, "earlier")
	})
	hrt.Go(func() { // later: waits for the earlier one, whatever its context says
		hrt.Park(func() bool { return released })
		order = append(order, "later")
	})
	stopping()
	cancel()
	done := make(chan struct{})
	go func() { host.Settle(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second): //blis:wallclock-ok a test's hang guard
		t.Fatal("Stop deadlocked: the earlier goroutine was held back for a later one waiting on it")
	}
	if len(order) != 2 || order[0] != "earlier" || order[1] != "later" {
		t.Fatalf("shutdown order %v, want [earlier later]", order)
	}
	if n := host.Live(); n != 0 {
		t.Errorf("%d goroutines live after Stop", n)
	}
}

// TestAdaptedClock: the clock the router reads is the host's: Now, timers and tickers.
func TestAdaptedClock(t *testing.T) {
	host := embedtest.NewRuntime(1)
	hrt, _ := adaptRuntime(host)
	if !hrt.Hosted() {
		t.Fatal("an adapted runtime is hosted")
	}
	if _, ok := hrt.(hosting.RoundTripper); !ok {
		t.Error("a host that serves HTTP serves the router's")
	}
	clk := hrt.Clock()
	start := clk.Now()
	timer := clk.NewTimer(time.Second)
	ticker := clk.NewTicker(300 * time.Millisecond)
	host.Advance(time.Second)
	if got := clk.Since(start); got != time.Second {
		t.Errorf("Since = %v, want 1s", got)
	}
	select {
	case <-timer.C():
	default:
		t.Error("the timer did not fire at its time")
	}
	if len(ticker.C()) != 1 {
		t.Error("the ticker did not tick")
	}
	ticker.Stop()
	var _ contract.Runtime = host
}
