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
	"fmt"
	"net/http"
	"sync"
	"time"

	contract "github.com/inference-sim/blis-schemas/embed"
	"k8s.io/utils/clock"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
)

// hostRuntime is the router's hosting.Runtime on a host's embed.Runtime. The router's
// components keep reading hosting.Runtime; the host implements only the contract.
//
// It also orders the router's shutdown. Some router loops wait for the goroutines they started
// without parking (the flow-control processor's sync.WaitGroup on its cleanup sweep), so when Stop
// readies every loop at once, a parent resumed before its children would block the host forever.
// Once stopping, a parked goroutine is therefore resumed only after every goroutine started after
// it has finished: children end before their parents, whatever order the host resumes them in.
// This relies on the contract's baton model (one router goroutine runs at a time), which tells
// the adapter which goroutine is parking.
type hostRuntime struct {
	rt  contract.Runtime
	clk hostClock

	mu       sync.Mutex
	nextID   int
	current  int          // the goroutine running now; 0 for the host's
	live     map[int]bool // goroutines started with Go that have not finished
	stopping bool
}

// hostServingRuntime is hostRuntime for a host that serves the router's outbound HTTP.
type hostServingRuntime struct {
	*hostRuntime
	rt contract.RoundTripper
}

var (
	_ hosting.Runtime      = (*hostRuntime)(nil)
	_ hosting.RoundTripper = hostServingRuntime{}
)

// adaptRuntime returns rt as a hosting.Runtime: Hosted, with a clock built from rt's timers and
// tickers, and serving outbound HTTP through rt when rt implements embed.RoundTripper. stop
// starts the shutdown order.
func adaptRuntime(rt contract.Runtime) (_ hosting.Runtime, stop func()) {
	h := &hostRuntime{rt: rt, clk: hostClock{rt: rt}, live: map[int]bool{}}
	if rtt, ok := rt.(contract.RoundTripper); ok {
		return hostServingRuntime{hostRuntime: h, rt: rtt}, h.stop
	}
	return h, h.stop
}

func (h hostServingRuntime) RoundTrip(r *http.Request) (*http.Response, error) {
	return h.rt.RoundTrip(r)
}

func (h *hostRuntime) Now() time.Time          { return h.rt.Now() }
func (h *hostRuntime) Clock() clock.WithTicker { return h.clk }
func (h *hostRuntime) Hosted() bool            { return true }

func (h *hostRuntime) Go(fn func()) {
	h.mu.Lock()
	h.nextID++
	id, parent := h.nextID, h.current
	h.live[id] = true
	h.mu.Unlock()
	h.rt.Go(func() {
		h.setCurrent(id)
		defer func() {
			h.mu.Lock()
			delete(h.live, id)
			h.mu.Unlock()
		}()
		fn()
	})
	h.setCurrent(parent)
}

func (h *hostRuntime) Park(ready func() bool) {
	h.mu.Lock()
	id := h.current
	h.mu.Unlock()
	h.rt.Park(func() bool { return ready() && h.mayResume(id) })
	h.setCurrent(id)
}

func (h *hostRuntime) setCurrent(id int) {
	h.mu.Lock()
	h.current = id
	h.mu.Unlock()
}

// mayResume reports whether goroutine id may resume: always, until the router stops; then only
// once every goroutine started after it has finished.
func (h *hostRuntime) mayResume(id int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopping {
		return true
	}
	for other := range h.live { // a membership test: order does not matter
		if other > id {
			return false
		}
	}
	return true
}

// stop starts the router's shutdown order (see hostRuntime).
func (h *hostRuntime) stop() {
	h.mu.Lock()
	h.stopping = true
	h.mu.Unlock()
}

func (h *hostRuntime) Every(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) {
	h.rt.Every(ctx, name, interval, fn)
}

func (h *hostRuntime) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return h.rt.WithTimeout(parent, d)
}

func (h *hostRuntime) WithDeadlineCause(parent context.Context, deadline time.Time, cause error) (context.Context, context.CancelFunc) {
	return h.rt.WithDeadlineCause(parent, deadline, cause)
}

func (h *hostRuntime) Rand(stream string) hosting.Rand { return h.rt.Rand(stream) }

func (h *hostRuntime) Local(key any, init func() any) any { return h.rt.Local(key, init) }

// hostClock is a clock.WithTicker on the host's timers and tickers.
type hostClock struct{ rt contract.Runtime }

var _ clock.WithTicker = hostClock{}

func (c hostClock) Now() time.Time                         { return c.rt.Now() }
func (c hostClock) Since(t time.Time) time.Duration        { return c.rt.Now().Sub(t) }
func (c hostClock) After(d time.Duration) <-chan time.Time { return c.rt.NewTimer(d).C() }
func (c hostClock) Tick(d time.Duration) <-chan time.Time  { return c.rt.NewTicker(d).C() }
func (c hostClock) NewTimer(d time.Duration) clock.Timer   { return c.rt.NewTimer(d) }
func (c hostClock) NewTicker(d time.Duration) clock.Ticker { return c.rt.NewTicker(d) }

// Sleep is never valid on a hosted runtime: time passes only when the host advances it, so a
// blocking sleep would never return.
func (c hostClock) Sleep(d time.Duration) {
	panic(fmt.Sprintf("embed: router called Sleep(%v): blocking sleeps are not supported in a hosted runtime", d))
}
