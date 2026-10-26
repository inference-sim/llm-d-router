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
	"errors"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	contract "github.com/inference-sim/blis-schemas/embed"

	errcommon "github.com/llm-d/llm-d-router/pkg/common/error"
	"github.com/llm-d/llm-d-router/pkg/common/routing"
	fwkrc "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	"github.com/llm-d/llm-d-router/pkg/epp/handlers"
	"github.com/llm-d/llm-d-router/pkg/epp/metadata"
)

// schedulerFallbackMsg starts the Director's message for a scheduler failure that carries no
// typed error (requestcontrol/director.go). It shares ResourceExhausted with admission denials but
// is not one.
const schedulerFallbackMsg = "failed to find target endpoint"

// flight is one request from Route until its response ends.
type flight struct {
	r      *Router
	id     string
	reqCtx *handlers.RequestContext
	ctx    context.Context
	cancel context.CancelFunc
	// evict is the router's in-flight eviction signal, taken when the request is dispatched; nil
	// without eviction.
	evict chan struct{}
	done  bool
}

var _ contract.Flight = (*flight)(nil)

// requestContext is a request's context: the router's values (its hosting runtime, its logger)
// with the host's request lifetime. Cancelling the host's context cancels the request's at once,
// on the canceller's goroutine, when the host's is a context.WithCancel context (or has none).
type requestContext struct {
	context.Context                 // the host's: lifetime and cancellation
	values          context.Context // the router's
}

func (c requestContext) Value(key any) any {
	if v := c.Context.Value(key); v != nil {
		return v
	}
	return c.values.Value(key)
}

// Route implements embed.Router. It runs the request phase exactly as the router's ext_proc
// server does for a request whose headers and body have arrived: the header handler, the body
// parser, then the Director on a goroutine of the runtime, which may wait in flow control. A
// failure before the Director is decided before Route returns.
func (r *Router) Route(ctx context.Context, req contract.Request, done func(contract.Flight, contract.Decision, *contract.Rejection)) {
	f := &flight{r: r, id: req.ID}
	f.ctx, f.cancel = context.WithCancel(requestContext{Context: ctx, values: r.ctx})
	f.reqCtx = &handlers.RequestContext{
		Request:  &handlers.Request{ID: req.ID, Headers: map[string]string{}, Metadata: map[string]any{}},
		Response: &handlers.Response{Headers: map[string]string{}},
	}
	r.flights[req.ID] = f
	if err := r.server.HandleRequestHeaders(f.ctx, f.reqCtx, requestHeaders(headerMap(requestHeaderMap(req)))); err != nil {
		r.decide(f, err, done)
		return
	}
	f.reqCtx.Request.RawBody = req.Body
	f.reqCtx.RequestSize = len(req.Body)
	parser, err := r.emb.Parsers.Resolve(req.Path)
	if err != nil {
		r.decide(f, errcommon.Error{Code: errcommon.BadRequest, Msg: err.Error()}, done)
		return
	}
	f.reqCtx.Parser = parser
	parsed, err := parser.ParseRequest(f.ctx, req.Body, f.reqCtx.Request.Headers)
	if err != nil {
		r.decide(f, errcommon.Error{Code: errcommon.BadRequest, Msg: err.Error()}, done)
		return
	}
	hostRT(r.ctx).Go(func() {
		_, err := r.emb.Director.HandleRequest(f.ctx, f.reqCtx, parsed.Body)
		if r.stopped {
			return // a request undecided when the router stops is never decided
		}
		r.decide(f, err, done)
	})
}

// decide reports the router's decision on f to the host.
func (r *Router) decide(f *flight, err error, done func(contract.Flight, contract.Decision, *contract.Rejection)) {
	if err != nil {
		kind, msg := classify(err)
		done(nil, contract.Decision{}, &contract.Rejection{Kind: kind, Msg: msg})
		f.end()
		return
	}
	rc := f.reqCtx
	d := contract.Decision{
		Endpoint:      strings.Split(rc.TargetEndpoint, ",")[0],
		Scores:        maps.Clone(rc.TargetEndpointScores),
		IncomingModel: rc.IncomingModelName,
		TargetModel:   rc.TargetModelName,
	}
	// The disaggregation profile handler sets it with routing.SetRoutingHeader, which writes the
	// canonical name and its legacy alias with one value.
	if hp, ok := metadata.GetLowerCaseHeaderValue(rc.Request.Headers, routing.PrefillEndpointHeader); ok && hp != "" {
		d.Prefill = strings.Split(hp, ",")[0]
	}
	for _, name := range routing.HeaderNames(routing.EncoderEndpointsHeader) {
		hp, ok := metadata.GetLowerCaseHeaderValue(rc.Request.Headers, name)
		if !ok || hp == "" {
			continue
		}
		// Every encoder the router chose, in its order, as the encode sidecar uses them all.
		for _, target := range strings.Split(hp, ",") {
			d.Encoders = append(d.Encoders, strings.TrimSpace(target))
		}
		break
	}
	if r.emb.RequestEvictor != nil {
		f.evict = r.emb.RequestEvictor.EvictionRegistry().Get(f.id)
	}
	done(f, d, nil)
}

// classify maps the router's rejection of a request onto a RejectKind. The outcomes are an
// explicit allow-list, keyed on the router's status code and its x-llm-d-request-dropped-reason:
//
//   - NoEndpoints, TTLExpired, Saturated, ContextCancelled: the drop reasons of the same names;
//   - Admission: ResourceExhausted with no drop reason (the legacy admission controller shedding
//     a sheddable request, or an admitter plugin), except the scheduler's untyped failure;
//   - Unavailable: ServiceUnavailable with no drop reason.
//
// Anything else is Unclassified, with the error's full text.
func classify(err error) (contract.RejectKind, string) {
	var e errcommon.Error
	if !errors.As(err, &e) {
		return contract.Unclassified, err.Error()
	}
	switch dropped, hasReason := e.Headers[errcommon.RequestDroppedReasonHeaderKey]; {
	case hasReason:
		switch errcommon.RequestDroppedReason(dropped) {
		case errcommon.RequestDroppedReasonNoEndpoints:
			return contract.NoEndpoints, e.Msg
		case errcommon.RequestDroppedReasonTTLExpired:
			return contract.TTLExpired, e.Msg
		case errcommon.RequestDroppedReasonSaturated:
			return contract.Saturated, e.Msg
		case errcommon.RequestDroppedReasonContextCancelled:
			return contract.ContextCancelled, e.Msg
		}
	case e.Code == errcommon.ResourceExhausted && !strings.HasPrefix(e.Msg, schedulerFallbackMsg):
		return contract.Admission, e.Msg
	case e.Code == errcommon.ServiceUnavailable:
		return contract.Unavailable, e.Msg
	}
	return contract.Unclassified, err.Error()
}

// requestHeaderMap is the request headers a client sends for req.
func requestHeaderMap(req contract.Request) map[string]string {
	h := map[string]string{
		":method":      "POST",
		":path":        req.Path,
		"content-type": "application/json",
		"x-request-id": req.ID,
	}
	if req.Objective != "" {
		h[metadata.ObjectiveKey] = req.Objective
	}
	if req.FairnessID != "" {
		h[metadata.FlowFairnessIDKey] = req.FairnessID
	}
	if req.TTFTSLO > 0 {
		// The header is whole milliseconds; round up so a sub-millisecond target stays a target.
		h[metadata.TTFTSLOHeaderKey] = strconv.FormatInt(int64((req.TTFTSLO+time.Millisecond-1)/time.Millisecond), 10)
	}
	if req.TTL > 0 {
		h[metadata.InferenceTTLHeaderKey] = req.TTL.String()
	}
	return h
}

func requestHeaders(h *configPb.HeaderMap) *extProcPb.ProcessingRequest_RequestHeaders {
	return &extProcPb.ProcessingRequest_RequestHeaders{RequestHeaders: &extProcPb.HttpHeaders{Headers: h}}
}

func responseHeaders(h *configPb.HeaderMap) *extProcPb.ProcessingRequest_ResponseHeaders {
	return &extProcPb.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extProcPb.HttpHeaders{Headers: h}}
}

// headerMap is h as Envoy hands headers to the router, in sorted key order.
func headerMap(h map[string]string) *configPb.HeaderMap {
	keys := make([]string, 0, len(h))
	for k := range h { // sorted below
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := &configPb.HeaderMap{}
	for _, k := range keys {
		m.Headers = append(m.Headers, &configPb.HeaderValue{Key: k, RawValue: []byte(h[k])})
	}
	return m
}

// ResponseHeaders implements embed.Flight.
func (f *flight) ResponseHeaders(h map[string]string) map[string]string {
	f.r.server.HandleResponseHeaders(f.ctx, f.reqCtx, responseHeaders(headerMap(h)))
	return f.reqCtx.Response.Headers
}

// ResponseBody implements embed.Flight.
func (f *flight) ResponseBody(chunk []byte, endOfStream bool) {
	f.r.server.HandleResponseBody(f.ctx, f.reqCtx, chunk, endOfStream)
	if endOfStream {
		f.end()
	}
}

// Terminate implements embed.Flight: the termination the ext_proc server reports when a stream
// breaks.
func (f *flight) Terminate(cause contract.Termination) {
	f.reqCtx.TerminationCause = terminationCause(cause)
	f.r.emb.Director.HandleResponseBody(context.WithoutCancel(f.ctx), f.reqCtx, true)
	f.end()
}

func terminationCause(c contract.Termination) fwkrc.TerminationCause {
	switch c {
	case contract.ClientDisconnect:
		return fwkrc.TerminationCauseClientDisconnect
	case contract.Evicted:
		return fwkrc.TerminationCauseEvicted
	default:
		return fwkrc.TerminationCauseError
	}
}

// EvictionSignal implements embed.Flight.
func (f *flight) EvictionSignal() <-chan struct{} {
	if f.evict == nil {
		return nil
	}
	return f.evict
}

// end ends f: its context is cancelled and the router forgets it.
func (f *flight) end() {
	if f.done {
		return
	}
	f.done = true
	f.cancel()
	delete(f.r.flights, f.id)
}
