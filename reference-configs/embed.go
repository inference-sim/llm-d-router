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

// Package referenceconfigs carries the reference EndpointPickerConfig files and the policies.json
// index that describes them, so a program that embeds the router can read them without a
// checkout. go:embed cannot reach across modules, so the files are embedded here, next to them.
//
// Import it as github.com/llm-d/llm-d-router/reference-configs. Every path policies.json names
// is a path in FS.
package referenceconfigs

import "embed"

// FS holds policies.json and every reference configuration file.
//
//go:embed *.yaml guides/*.yaml policies.json
var FS embed.FS
