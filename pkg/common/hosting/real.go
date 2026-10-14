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

package hosting

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// Real is the Runtime of a router running as a process: the wall clock, goroutines, tickers and
// process-wide randomness.
var Real Runtime = realRuntime{}

type realRuntime struct{}

var realLocals sync.Map

func (realRuntime) Now() time.Time { return time.Now() } //blis:wallclock-ok Real runtime

func (realRuntime) Clock() clock.WithTicker { return clock.RealClock{} }

func (realRuntime) Go(fn func()) { go fn() }

func (realRuntime) Every(ctx context.Context, _ string, interval time.Duration, fn func(context.Context)) {
	go func() {
		ticker := time.NewTicker(interval) //blis:wallclock-ok Real runtime
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fn(ctx)
			}
		}
	}()
}

func (realRuntime) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout) //blis:wallclock-ok Real runtime
}

func (realRuntime) WithDeadlineCause(parent context.Context, deadline time.Time, cause error) (context.Context, context.CancelFunc) {
	return context.WithDeadlineCause(parent, deadline, cause) //blis:wallclock-ok Real runtime
}

func (realRuntime) Park(func() bool) {}

func (realRuntime) Rand(string) Rand { return globalRand{} }

func (realRuntime) Local(key any, init func() any) any {
	if v, ok := realLocals.Load(key); ok {
		return v
	}
	v, _ := realLocals.LoadOrStore(key, init())
	return v
}

func (realRuntime) Hosted() bool { return false }

// globalRand draws from the process-wide math/rand/v2 source, which is safe for concurrent use.
type globalRand struct{}

func (globalRand) Float64() float64                   { return rand.Float64() } //#nosec G404 -- routing randomness, not security-sensitive
func (globalRand) IntN(n int) int                     { return rand.IntN(n) }   //#nosec G404 -- routing randomness, not security-sensitive
func (globalRand) Shuffle(n int, swap func(i, j int)) { rand.Shuffle(n, swap) } //#nosec G404 -- routing randomness, not security-sensitive
