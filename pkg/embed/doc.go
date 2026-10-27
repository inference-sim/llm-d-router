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

// Package embed runs the router inside a host process (a discrete-event simulator) through the
// contract in github.com/inference-sim/blis-schemas/embed. New is that contract's Constructor.
//
// This package is the fork's versioned surface: a host imports only New and the blis-schemas
// types. Everything here that knows the router's internals (its flag set, configuration schema,
// request handlers, Director, datastore, pod reconciler, flow-control registry and KV-event
// producers) stays behind New, so an upstream release that restructures those internals is
// absorbed here and reaches the host as a pin bump.
//
// The package changes how the router is hosted, never how it decides: given the same inputs, the
// router's plugins decide as they do in a deployment. It assembles the router through
// runner.Embed, the runner's own startup path, on a hosting.Runtime adapted from the host's
// embed.Runtime, and feeds it requests, endpoints, responses and KV events the way the ext_proc
// server, the Kubernetes reconcilers and the KV-event subscriber would. A request carries only
// the headers embed.Request describes (method, path, content type, request ID, objective,
// fairness ID, TTFT objective, TTL); a plugin that reads any other request header sees it absent,
// as it would for a client that sends none.
package embed
