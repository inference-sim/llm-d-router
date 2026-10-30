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
	"cmp"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	contract "github.com/inference-sim/blis-schemas/embed"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"

	reqcommon "github.com/llm-d/llm-d-router/pkg/common/request"
	runserver "github.com/llm-d/llm-d-router/pkg/epp/server"
)

// inertFlags are router flags that change how a deployed router is served or observed
// (transport, TLS, replicas, shutdown, telemetry), never what it decides. A hosted router has
// none of that, so they are accepted and have no effect.
var inertFlags = map[string]bool{
	"ha-enable-leader-election": true, "ha-lease-name": true, "ha-lease-duration": true, "ha-renew-deadline": true,
	"ha-retry-period": true, "drain-timeout": true, "grpc-max-recv-msg-size": true, "grpc-max-send-msg-size": true,
	"grpc-port": true, "grpc-health-port": true, "metrics-port": true, "emit-endpoint-scores": true,
	"refresh-prometheus-metrics-interval": true, "tracing": true, "health-checking": true, "enable-pprof": true,
	"cert-path": true, "enable-cert-reload": true, "enable-grpc-stream-metrics": true, "fairness-id-metric-label-limit": true,
	"secure-serving": true, "tls-min-version": true, "metrics-endpoint-auth": true, "metrics-client-ca-file": true,
	"metrics-cert-dir": true, "tls-cipher-suites": true,
}

// metricFlags name the engine series the router reads when its configuration does not.
var metricFlags = []string{
	"total-queued-requests-metric", "total-running-requests-metric", "kv-cache-usage-percentage-metric",
	"lora-info-metric", "cache-info-metric",
}

// parsedFlags are the router's command-line flags, parsed with its own flag set, as the EPP
// binary parses them.
type parsedFlags struct {
	opts        *runserver.Options
	inert       []string
	metricFlags map[string]string
}

func parseFlags(args []string) (*parsedFlags, error) {
	opts := runserver.NewOptions()
	fs := pflag.NewFlagSet("epp", pflag.ContinueOnError)
	opts.AddFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("EPP flags: %w", err)
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("EPP flags: unexpected arguments %q", fs.Args())
	}
	out := &parsedFlags{opts: opts}
	for _, name := range metricFlags {
		if fs.Changed(name) {
			if out.metricFlags == nil {
				out.metricFlags = map[string]string{}
			}
			out.metricFlags[name] = fs.Lookup(name).Value.String()
		}
	}
	fs.Visit(func(f *pflag.Flag) {
		if inertFlags[f.Name] {
			out.inert = append(out.inert, "--"+f.Name)
		}
	})
	sort.Strings(out.inert)
	if err := opts.Complete(); err != nil {
		return nil, fmt.Errorf("EPP flags: %w", err)
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("EPP flags: %w", err)
	}
	return out, nil
}

// configBytes returns the EndpointPickerConfig the router loads: the one the flags name, or
// cfg, which the router is then given as --config-text.
func configBytes(opts *runserver.Options, cfg []byte) ([]byte, error) {
	if len(cfg) > 0 {
		if opts.ConfigText != "" || opts.ConfigFile != "" {
			return nil, errors.New("EPP flags: the configuration is given both by a flag (--config-file or --config-text) and in the embed configuration")
		}
		opts.ConfigText = string(cfg)
		return cfg, nil
	}
	if opts.ConfigFile != "" {
		b, err := os.ReadFile(opts.ConfigFile)
		if err != nil {
			return nil, fmt.Errorf("EPP flag --config-file: %w", err)
		}
		return b, nil
	}
	return []byte(opts.ConfigText), nil
}

// pluginSpec is a plugin as the configuration states it.
type pluginSpec struct {
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Parameters map[string]any `json:"parameters"`
}

func (p pluginSpec) name() string { return cmp.Or(p.Name, p.Type) }

// configDoc is the part of an EndpointPickerConfig the embedding reads. The router's own loader
// validates the whole document; a document this cannot read is reported by that loader.
type configDoc struct {
	// plugins are the configuration's plugins; pluginsOK is false when the document does not
	// parse as a list of plugins (no requirement but the flags' is then reported).
	plugins   []pluginSpec
	pluginsOK bool
	// topology is the document as topology-aware routing reads it, with the scheduling
	// profiles; topologyOK is false when it does not parse so (no topology fact is reported).
	topology   topologyDoc
	topologyOK bool
}

type topologyDoc struct {
	Plugins            []pluginSpec `json:"plugins"`
	SchedulingProfiles []struct {
		Name string `json:"name"`
	} `json:"schedulingProfiles"`
}

// readConfig reads raw, each view on its own: a document may parse as one and not the other.
func readConfig(raw []byte) configDoc {
	var doc configDoc
	var plugins struct {
		Plugins []pluginSpec `json:"plugins"`
	}
	if len(raw) == 0 || yaml.Unmarshal(raw, &plugins) == nil {
		doc.plugins, doc.pluginsOK = plugins.Plugins, true
	}
	doc.topologyOK = yaml.Unmarshal(raw, &doc.topology) == nil
	return doc
}

// Defaults of the configuration elements Requirements reports, as the plugins apply them.
const (
	defaultEngineLabelKey = "llm-d.ai/engine-type"
	defaultEngine         = "vllm"
	defaultMetricsPath    = "/metrics"
	defaultKVBlockSize    = 16 // kvblock.TokenProcessorConfig's default
	defaultKVEngineType   = "vllm"
	extractorType         = "core-metrics-extractor"
)

// engineSpecKeys are an engine mapping's series keys, in the order they are reported.
var engineSpecKeys = []string{"queuedRequestsSpec", "runningRequestsSpec", "kvUsageSpec", "loraSpec", "cacheInfoSpec", "cacheBlockSizeSpec", "cacheNumBlocksSpec"}

// requirements are the facts of a configuration a host compares with what it produces. A
// configuration that does not parse has none beyond its flags: the router's loader reports it.
func requirements(flags *parsedFlags, doc configDoc) contract.Requirements {
	req := contract.Requirements{MetricFlags: flags.metricFlags, InertFlags: flags.inert}
	if doc.topologyOK {
		req.Topology = topologyFacts(doc.topology)
	}
	if !doc.pluginsOK {
		return req
	}
	for _, p := range doc.plugins {
		switch p.Type {
		case extractorType:
			req.Extractors = append(req.Extractors, extractorFacts(p.name(), p.Parameters))
		case "metrics-data-source":
			path, _ := p.Parameters["path"].(string)
			req.MetricsSources = append(req.MetricsSources, contract.MetricsSource{Name: p.name(), Path: cmp.Or(path, defaultMetricsPath)})
		case "precise-prefix-cache-producer":
			req.PrefixIndexes = append(req.PrefixIndexes, prefixIndexFacts(p.name(), p.Parameters))
		case "disagg-profile-handler":
			req.Disaggregates = true
		}
	}
	if len(req.Extractors) == 0 { // the router adds its default extractor
		e := extractorFacts(extractorType, nil)
		e.Implicit = true
		req.Extractors = []contract.MetricsExtractor{e}
	}
	return req
}

func extractorFacts(name string, params map[string]any) contract.MetricsExtractor {
	labelKey, _ := params["engineLabelKey"].(string)
	engine, _ := params["defaultEngine"].(string)
	e := contract.MetricsExtractor{Name: name, EngineLabelKey: cmp.Or(labelKey, defaultEngineLabelKey), DefaultEngine: cmp.Or(engine, defaultEngine)}
	list, _ := params["engineConfigs"].([]any)
	for _, ec := range list {
		m, ok := ec.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		mapping := contract.EngineMapping{Name: name}
		for _, key := range engineSpecKeys {
			if s, _ := m[key].(string); s != "" {
				if mapping.Series == nil {
					mapping.Series = map[string]string{}
				}
				mapping.Series[key] = s
			}
		}
		custom, _ := m["customMetrics"].([]any)
		for _, c := range custom {
			cm, _ := c.(map[string]any)
			s, _ := cm["metricSpec"].(string)
			optional, _ := cm["optional"].(bool)
			mapping.Custom = append(mapping.Custom, contract.CustomMetric{Series: s, Optional: optional})
		}
		e.Engines = append(e.Engines, mapping)
	}
	return e
}

func prefixIndexFacts(name string, params map[string]any) contract.PrefixIndex {
	ix := contract.PrefixIndex{Name: name, BlockSize: defaultKVBlockSize, EngineType: defaultKVEngineType}
	if tp, _ := params["tokenProcessorConfig"].(map[string]any); tp != nil {
		for _, key := range []string{"blockSize", "blockSizeTokens"} { // blockSize is the deprecated spelling
			if v, ok := tp[key].(float64); ok && v > 0 {
				ix.BlockSize = int(v)
			}
		}
	}
	if events, _ := params["kvEventsConfig"].(map[string]any); events != nil {
		if engine, _ := events["engineType"].(string); engine != "" {
			ix.EngineType = engine
		}
	}
	return ix
}

func topologyFacts(doc topologyDoc) contract.Topology {
	var t contract.Topology
	for _, p := range doc.Plugins {
		switch p.Type {
		case "topology-extractor":
			t.Requested = true
			t.Extractors = append(t.Extractors, topologyExtractorFacts(p.name(), p.Parameters))
		case "topology-affinity-filter", "topology-affinity-scorer", "topology-stamp-handler":
			t.Requested = true
		}
	}
	return t
}

func topologyExtractorFacts(name string, params map[string]any) contract.TopologyExtractor {
	label := func(key, def string) string {
		v, _ := params[key].(string)
		return cmp.Or(v, def)
	}
	return contract.TopologyExtractor{
		Name:     name,
		Hostname: label("hostname", "kubernetes.io/hostname"),
		Rack:     label("rack", "topology.kubernetes.io/rack"),
		Zone:     label("zone", "topology.kubernetes.io/zone"),
		Region:   label("region", "topology.kubernetes.io/region"),
	}
}

// hostingRefusal refuses a configuration a hosted router cannot run as configured, for reasons
// that are facts of the hosting, not of what the host's engines publish:
//   - a topology consumer with a peer topology header needs a coordinator, and an embedding runs
//     one router; a topology stamp handler must use the peer topology header and an existing
//     profile; a topology consumer needs the topology extractor it names;
//   - a token-producer whose vLLM backend discovers endpoints probes pods on its own schedule;
//   - a precise-prefix-cache-producer whose index is in Redis reaches the network, and the
//     cost-aware index ranges a sync.Map, so its order is not reproducible;
//   - a prefix-cache-affinity-filter whose load gate reads TTFT from the latency predictor
//     (ttftSource latencyPredictor) consumes an external service; its default source,
//     prefillThroughput, reads the in-flight load.
func hostingRefusal(doc configDoc) error {
	if doc.topologyOK {
		if err := topologyRefusal(doc.topology); err != nil {
			return err
		}
	}
	if !doc.pluginsOK {
		return nil
	}
	for _, p := range doc.plugins {
		switch p.Type {
		case "token-producer":
			if v, _ := p.Parameters["vllm"].(map[string]any); v != nil && enabled(v["endpointDiscovery"]) {
				return fmt.Errorf("plugin %q discovers render endpoints (vllm.endpointDiscovery), probing pods on its own "+
					"schedule outside the simulation; give its vLLM backend a fixed url, or use the estimate backend", p.name())
			}
		case "prefix-cache-affinity-filter":
			if src, _ := p.Parameters["ttftSource"].(string); src == "latencyPredictor" {
				return fmt.Errorf("plugin %q reads TTFT from the latency predictor (ttftSource latencyPredictor), an "+
					"external service; use ttftSource prefillThroughput, which reads the in-flight load", p.name())
			}
		case "precise-prefix-cache-producer":
			indexer, _ := p.Parameters["indexerConfig"].(map[string]any)
			index, _ := indexer["kvBlockIndexConfig"].(map[string]any)
			if enabled(index["redisConfig"]) {
				return fmt.Errorf("plugin %q keeps its index in Redis, an external service", p.name())
			}
			if enabled(index["costAwareMemoryConfig"]) {
				return fmt.Errorf("plugin %q uses the cost-aware index, whose lookups range a sync.Map in no fixed order", p.name())
			}
		}
	}
	return nil
}

// topologyRefusal admits only the single-router form of topology-aware routing. It verifies
// static producer references; a filter used before peer selection still follows the router's
// intentional fail-open semantics.
func topologyRefusal(doc topologyDoc) error {
	extractors := map[string]bool{}
	profiles := map[string]bool{}
	for _, profile := range doc.SchedulingProfiles {
		profiles[profile.Name] = true
	}
	type consumerRef struct{ plugin, producer string }
	var consumers []consumerRef
	for _, p := range doc.Plugins {
		switch p.Type {
		case "topology-extractor":
			extractors[p.name()] = true
		case "topology-affinity-filter", "topology-affinity-scorer":
			producer, _ := p.Parameters["topologyProducerName"].(string)
			consumers = append(consumers, consumerRef{p.Type, cmp.Or(producer, "topology-extractor")})
			if header, _ := p.Parameters["peerTopologyHeader"].(string); header != "" {
				return fmt.Errorf("%s peerTopologyHeader requires a coordinator; BLIS embeds one EPP", p.Type)
			}
		case "topology-stamp-handler":
			producer, _ := p.Parameters["topologyProducerName"].(string)
			consumers = append(consumers, consumerRef{p.Type, cmp.Or(producer, "topology-extractor")})
			if header, _ := p.Parameters["headerName"].(string); header != "" && !strings.EqualFold(header, reqcommon.PeerTopologyHeaderKey) {
				return fmt.Errorf("%s headerName %q is not supported; use %q", p.Type, header, reqcommon.PeerTopologyHeaderKey)
			}
			if profile, _ := p.Parameters["profileName"].(string); profile != "" && !profiles[profile] {
				return fmt.Errorf("%s profileName %q has no scheduling profile", p.Type, profile)
			}
		}
	}
	for _, c := range consumers {
		if !extractors[c.producer] {
			return fmt.Errorf("%s needs topology-extractor %q; without it the plugin has no topology to use", c.plugin, c.producer)
		}
	}
	return nil
}

// enabled reports whether a configuration value switches a feature on: true, or a non-empty
// block of settings.
func enabled(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case map[string]any:
		return len(v) > 0
	case nil:
		return false
	default:
		return true
	}
}
