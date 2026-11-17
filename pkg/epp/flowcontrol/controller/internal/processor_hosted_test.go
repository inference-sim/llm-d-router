/*
Copyright 2025 The Kubernetes Authors.
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
package internal

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
)

// batonRuntime hosts goroutines as a one-at-a-time simulator does (RTR-7): a goroutine runs only
// while it holds the baton, and gives it up only in Park. Everything else is the real runtime's.
//
// A goroutine started with Go resumes after a longer pause than the one that started it, as a host
// that resumes the oldest parked goroutine first does: the parent is back before its child.
type batonRuntime struct {
	hosting.Runtime
	baton    sync.Mutex
	children sync.Map // goroutine ids started with Go
}

// gid is the calling goroutine's id, read from its stack header.
func gid() string {
	var buf [64]byte
	f := strings.Fields(string(buf[:runtime.Stack(buf[:], false)]))
	return f[1]
}

func (r *batonRuntime) Hosted() bool { return true }

func (r *batonRuntime) Go(fn func()) {
	go func() {
		r.children.Store(gid(), true)
		r.baton.Lock()
		defer r.baton.Unlock()
		fn()
	}()
}

func (r *batonRuntime) Park(ready func() bool) {
	pause := time.Millisecond
	if _, child := r.children.Load(gid()); child {
		pause = 25 * time.Millisecond
	}
	for !ready() {
		r.baton.Unlock()
		time.Sleep(pause) //blis:wallclock-ok a test host yielding the baton
		r.baton.Lock()
	}
}

// TestRun_ShutdownCompletesWhenTheHostRunsOneGoroutineAtATime: when the processor's loop stops, it
// must wait for its cleanup sweep in a way a one-goroutine-at-a-time host can see. A bare
// WaitGroup.Wait keeps the baton while the sweep, which needs it to return, never runs.
func TestRun_ShutdownCompletesWhenTheHostRunsOneGoroutineAtATime(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, testCleanupTick)
	rt := &batonRuntime{Runtime: hosting.Real}
	ctx, cancel := context.WithCancel(hosting.Into(context.Background(), rt))
	done := make(chan struct{})
	rt.baton.Lock()
	go func() {
		defer close(done)
		rt.baton.Lock()
		defer rt.baton.Unlock()
		h.processor.Run(ctx)
	}()
	rt.baton.Unlock()
	time.Sleep(20 * time.Millisecond) //blis:wallclock-ok let the loop reach its first Park
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second): //blis:wallclock-ok a test's hang guard
		t.Fatal("the processor did not stop: it waits for its cleanup sweep without parking")
	}
}
