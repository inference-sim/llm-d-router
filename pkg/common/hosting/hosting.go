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

// Package hosting is the seam through which the router reads time, starts
// background work, blocks on other work, and draws randomness.
//
// A Runtime travels on the context. Code that has no Runtime on its context
// gets Real, which is the wall clock, goroutines, tickers and process-wide
// randomness. A host that embeds the router (for example a discrete-event
// simulator) installs its own Runtime with Into before it constructs any
// router component, and every router decision then reads the host's clock and
// randomness and runs on the host's schedule.
//
// The seam changes how the router is hosted, never what it decides: the Real
// Runtime reproduces the behavior the router has without the seam.
package hosting

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/utils/clock"
)

// Runtime is the hosting environment of a router instance.
type Runtime interface {
	// Now returns the current time.
	Now() time.Time

	// Clock returns the runtime clock for components that take a clock.WithTicker.
	Clock() clock.WithTicker

	// Go runs fn concurrently with the caller. A hosted runtime may instead run fn to completion
	// before Go returns.
	Go(fn func())

	// Every calls fn every interval until ctx is done. The first call happens one interval after
	// Every is called. name identifies the loop to the runtime. Every does not block.
	Every(ctx context.Context, name string, interval time.Duration, fn func(context.Context))

	// WithTimeout is context.WithTimeout on the runtime clock.
	WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc)

	// WithDeadlineCause is context.WithDeadlineCause on the runtime clock.
	WithDeadlineCause(parent context.Context, deadline time.Time, cause error) (context.Context, context.CancelFunc)

	// Park is called by a goroutine immediately before it blocks waiting for work that runs
	// elsewhere in the router. ready reports whether that work has happened; the goroutine then
	// blocks only if ready returns false. A hosted runtime uses Park to hand control to the work
	// the goroutine waits for.
	Park(ready func() bool)

	// Rand returns the named random stream.
	Rand(stream string) Rand

	// Local returns the value stored under key, storing init() first if there is none. It holds
	// state that is process-wide under Real and per-runtime under a hosted runtime.
	Local(key any, init func() any) any

	// Hosted reports whether a host drives the router's background work. When true, loops that
	// the router would run on their own goroutines are driven by the host instead.
	Hosted() bool
}

// Rand is a source of random numbers.
type Rand interface {
	Float64() float64
	IntN(n int) int
	Shuffle(n int, swap func(i, j int))
}

type contextKey struct{}

// Into returns a copy of ctx that carries rt.
func Into(ctx context.Context, rt Runtime) context.Context {
	return context.WithValue(ctx, contextKey{}, rt)
}

// From returns the Runtime carried by ctx, or Real if ctx carries none or is nil.
func From(ctx context.Context) Runtime {
	if ctx != nil {
		if rt, ok := ctx.Value(contextKey{}).(Runtime); ok {
			return rt
		}
	}
	return Real
}

// Now returns From(ctx).Now().
func Now(ctx context.Context) time.Time {
	return From(ctx).Now()
}

// Since returns the time elapsed on the runtime clock of ctx since t.
func Since(ctx context.Context, t time.Time) time.Duration {
	return From(ctx).Now().Sub(t)
}

// RoundTripper is implemented by a Runtime that serves the router's outbound HTTP requests itself,
// for example a simulator that plays the model servers.
type RoundTripper interface {
	RoundTrip(*http.Request) (*http.Response, error)
}

// Transport returns base wrapped so that a request whose context carries a hosted Runtime that
// implements RoundTripper is served by that Runtime instead of the network.
func Transport(base http.RoundTripper) http.RoundTripper {
	return hostedTransport{base: base}
}

type hostedTransport struct{ base http.RoundTripper }

func (t hostedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if rt := From(r.Context()); rt.Hosted() {
		if server, ok := rt.(RoundTripper); ok {
			return server.RoundTrip(r)
		}
		// A hosted router never reaches the network: its runtime owns time and outbound I/O.
		return nil, fmt.Errorf("hosting: the hosted runtime does not serve HTTP; refusing %s %s", r.Method, r.URL)
	}
	return t.base.RoundTrip(r)
}

// CloseIdleConnections forwards to the base transport, as http.Client.CloseIdleConnections expects.
func (t hostedTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
