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
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/clock"
)

// fakeRuntime is a minimal hosted runtime; server, when set, makes it a RoundTripper.
type fakeRuntime struct {
	now time.Time
}

func (f fakeRuntime) Now() time.Time                                                      { return f.now }
func (f fakeRuntime) Clock() clock.WithTicker                                             { return nil }
func (f fakeRuntime) Go(fn func())                                                        { fn() }
func (f fakeRuntime) Park(func() bool)                                                    {}
func (f fakeRuntime) Rand(string) Rand                                                    { return globalRand{} }
func (f fakeRuntime) Local(_ any, init func() any) any                                    { return init() }
func (f fakeRuntime) Hosted() bool                                                        { return true }
func (f fakeRuntime) Every(context.Context, string, time.Duration, func(context.Context)) {}
func (f fakeRuntime) WithTimeout(p context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(p)
}
func (f fakeRuntime) WithDeadlineCause(p context.Context, _ time.Time, _ error) (context.Context, context.CancelFunc) {
	return context.WithCancel(p)
}

type servingRuntime struct{ fakeRuntime }

func (servingRuntime) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusTeapot, Body: io.NopCloser(strings.NewReader("hosted")), Request: r}, nil
}

type recordingTransport struct{ calls, closes int }

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("network")), Request: r}, nil
}
func (t *recordingTransport) CloseIdleConnections() { t.closes++ }

func TestFromDefaultsToReal(t *testing.T) {
	if From(context.Background()) != Real || From(nil) != Real { //nolint:staticcheck // a nil context is part of the contract
		t.Fatal("a context without a runtime must yield Real")
	}
	at := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx := Into(context.Background(), fakeRuntime{now: at})
	if !Now(ctx).Equal(at) || Since(ctx, at.Add(-time.Second)) != time.Second {
		t.Fatal("Now and Since must read the context's runtime")
	}
	if Real.Hosted() {
		t.Fatal("Real is not hosted")
	}
}

func TestTransport(t *testing.T) {
	base := &recordingTransport{}
	tr := Transport(base)
	get := func(ctx context.Context) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://10.0.0.1:8000/metrics", nil)
		return tr.RoundTrip(req)
	}

	// Real: the base transport, unchanged.
	if resp, err := get(context.Background()); err != nil || resp.StatusCode != http.StatusOK || base.calls != 1 {
		t.Fatalf("Real: %v %v calls=%d", resp, err, base.calls)
	}
	// A hosted runtime that serves HTTP serves it; the network is never used.
	if resp, err := get(Into(context.Background(), servingRuntime{})); err != nil || resp.StatusCode != http.StatusTeapot || base.calls != 1 {
		t.Fatalf("hosted server: %v %v calls=%d", resp, err, base.calls)
	}
	// A hosted runtime that does not serve HTTP is refused, never sent to the network.
	if _, err := get(Into(context.Background(), fakeRuntime{})); err == nil || base.calls != 1 {
		t.Fatalf("hosted without a server: err=%v calls=%d, want an error and no network call", err, base.calls)
	}
	tr.(interface{ CloseIdleConnections() }).CloseIdleConnections()
	if base.closes != 1 {
		t.Fatal("CloseIdleConnections must reach the base transport")
	}
}

func TestRealRuntime(t *testing.T) {
	ctx, cancel := Real.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if d, ok := ctx.Deadline(); !ok || time.Until(d) <= 0 {
		t.Fatal("Real.WithTimeout must set a wall-clock deadline")
	}
	cause := errors.New("cause")
	dctx, dcancel := Real.WithDeadlineCause(context.Background(), time.Now().Add(-time.Second), cause)
	defer dcancel()
	<-dctx.Done()
	if !errors.Is(context.Cause(dctx), cause) {
		t.Fatal("Real.WithDeadlineCause must report its cause")
	}
	done := make(chan struct{})
	Real.Go(func() { close(done) })
	<-done
	if v := Real.Local("k", func() any { return 1 }); v != 1 || Real.Local("k", func() any { return 2 }) != 1 {
		t.Fatal("Real.Local must keep the first value")
	}
	if n := Real.Rand("s").IntN(10); n < 0 || n >= 10 {
		t.Fatal("Real.Rand out of range")
	}
}
