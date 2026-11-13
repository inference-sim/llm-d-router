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

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	configapiv1 "github.com/llm-d/llm-d-router/apix/config/v1"
	"github.com/llm-d/llm-d-router/pkg/epp/config/loader"
	"github.com/llm-d/llm-d-router/pkg/epp/datastore"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/picker"
	"github.com/llm-d/llm-d-router/pkg/epp/scheduling"
	runserver "github.com/llm-d/llm-d-router/pkg/epp/server"
)

// referenceConfigs is the directory of reference configurations (see its README.md).
const referenceConfigs = "../../../reference-configs"

// refKnob is one parameter the index lists, swept (knobs) or accepted but without effect on a
// hosted decision (ineffective_parameters).
type refKnob struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Plugin        string `json:"plugin"`
	Parameter     string `json:"parameter"`
	Profile       string `json:"profile"`
	AppliesTo     string `json:"applies_to"`
	InFile        bool   `json:"in_file"`
	FileValue     any    `json:"file_value"`
	RouterDefault any    `json:"router_default"`
	HostedEffect  *bool  `json:"hosted_effect"`
	Reason        string `json:"reason"`
	Requires      []struct {
		ID    string `json:"id"`
		Value any    `json:"value"`
	} `json:"requires"`
}

// refIndex is the part of reference-configs/policies.json these tests read.
type refIndex struct {
	Policies []struct {
		Name  string `json:"name"`
		Files struct {
			Colocated *string `json:"colocated"`
			PD        *string `json:"pd"`
		} `json:"files"`
		Requires struct {
			EPPFlags []string `json:"epp_flags"`
		} `json:"requires"`
		Knobs                 []refKnob `json:"knobs"`
		IneffectiveParameters []refKnob `json:"ineffective_parameters"`
		PDDecider             *struct {
			InFile          string `json:"in_file"`
			CompleteVariant string `json:"complete_variant"`
		} `json:"pd_decider"`
	} `json:"policies"`
}

func readIndex(t *testing.T) refIndex {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(referenceConfigs, "policies.json"))
	require.NoError(t, err)
	var idx refIndex
	require.NoError(t, json.Unmarshal(b, &idx))
	require.NotEmpty(t, idx.Policies)
	return idx
}

// files returns the configuration files a policy offers, by shape.
func files(p struct {
	Colocated *string `json:"colocated"`
	PD        *string `json:"pd"`
}) map[string]string {
	out := map[string]string{}
	if p.Colocated != nil {
		out["colocated"] = *p.Colocated
	}
	if p.PD != nil {
		out["pd"] = *p.PD
	}
	return out
}

func readRef(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(referenceConfigs, name))
	require.NoError(t, err)
	return string(b)
}

// load parses configText the way the endpoint picker does at start-up (no configuration when
// configText is empty) and returns the runner, whose plugin handle holds every plugin the router
// made, and the effective configuration after the router's own defaults are applied.
func load(t *testing.T, configText string) (*Runner, *configapiv1.EndpointPickerConfig, error) {
	t.Helper()
	return loadWith(t, configText, false)
}

// loadWith is load with --allow-experimental-plugins set or not.
func loadWith(t *testing.T, configText string, experimental bool) (*Runner, *configapiv1.EndpointPickerConfig, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opts := runserver.NewOptions()
	opts.ConfigText = configText
	opts.PoolName = "reference-config-pool"
	// Reference configurations load on a default command line, except the ones that say in
	// requires.epp_flags that they need the experimental flag.
	opts.AllowExperimentalPlugins = experimental
	r := NewRunner()
	rawConfig, err := r.parseConfigurationPhaseOne(ctx, opts)
	if err != nil {
		return r, nil, err
	}
	ds := datastore.NewDatastore(ctx, r.setupMetricsCollection(opts))
	if _, err := r.parseConfigurationPhaseTwo(ctx, rawConfig, ds, opts.RefreshMetricsInterval); err != nil {
		return r, nil, err
	}
	if err := fwkplugin.ValidatePluginStability(r.PluginHandle, opts.AllowExperimentalPlugins); err != nil {
		return r, nil, err
	}
	return r, rawConfig, nil
}

// effective describes what a loaded configuration runs: every plugin the router instantiated
// (including those it added), the profiles with their weights, and the data layer.
func effective(t *testing.T, r *Runner, cfg *configapiv1.EndpointPickerConfig) string {
	t.Helper()
	all := r.PluginHandle.GetAllPluginsWithNames()
	plugins := make([]string, 0, len(all))
	for name, p := range all {
		plugins = append(plugins, name+":"+p.TypedName().Type)
	}
	slices.Sort(plugins)
	profiles, err := json.Marshal(cfg.SchedulingProfiles)
	require.NoError(t, err)
	data, err := json.Marshal(cfg.DataLayer)
	require.NoError(t, err)
	return strings.Join(plugins, ",") + "\n" + string(profiles) + "\n" + string(data)
}

// TestReferenceConfigsLoad: every file the index offers is accepted, unchanged, by the router's
// own configuration loader on the command line the index says it needs (a default one unless
// requires.epp_flags names --allow-experimental-plugins, and then the file is refused without
// it), and the directories hold no other file.
func TestReferenceConfigsLoad(t *testing.T) {
	idx := readIndex(t)
	offered := map[string]bool{}
	for _, p := range idx.Policies {
		needsExperimental := slices.Contains(p.Requires.EPPFlags, "--allow-experimental-plugins")
		for shape, name := range files(p.Files) {
			offered[name] = true
			t.Run(p.Name+"/"+shape, func(t *testing.T) {
				_, _, err := loadWith(t, readRef(t, name), needsExperimental)
				require.NoError(t, err, "%s no longer loads", name)
				if needsExperimental {
					_, _, err := load(t, readRef(t, name))
					require.Error(t, err, "%s loads without --allow-experimental-plugins, but the index says it needs it", name)
				}
			})
		}
	}
	onDisk, err := filepath.Glob(filepath.Join(referenceConfigs, "*.yaml"))
	require.NoError(t, err)
	guides, err := filepath.Glob(filepath.Join(referenceConfigs, "guides", "*.yaml"))
	require.NoError(t, err)
	onDisk = append(onDisk, guides...)
	for _, f := range onDisk {
		rel, err := filepath.Rel(referenceConfigs, f)
		require.NoError(t, err)
		require.True(t, offered[filepath.ToSlash(rel)], "%s is not in policies.json", f)
	}
	require.Len(t, onDisk, len(offered), "policies.json names a file that is not in the directory")
}

// TestLoaderRefusesABrokenReferenceConfig: the loading test above can fail. A file with a plugin
// type the router does not have, a parameter a plugin does not take, or a profile that names a
// plugin the file does not declare is refused.
func TestLoaderRefusesABrokenReferenceConfig(t *testing.T) {
	pd := readRef(t, "pd-prefix-based.yaml")
	for name, broken := range map[string]string{
		"unknown plugin type":     strings.Replace(pd, "type: queue-scorer", "type: queue-scorr", 1),
		"unknown parameter":       strings.Replace(pd, "nonCachedTokens: 16", "nonCachedToken: 16", 1),
		"profile names no plugin": strings.Replace(pd, "pluginRef: decode-filter", "pluginRef: decode-filtr", 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, pd, broken, "the mutation did not change the file")
			_, _, err := load(t, broken)
			require.Error(t, err)
		})
	}
}

// TestPDSampleIsTheRoutersSample: pd-prefix-based.yaml is the router's own P/D sample.
func TestPDSampleIsTheRoutersSample(t *testing.T) {
	read := func(path string) map[string]any {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, yaml.Unmarshal(b, &m))
		return m
	}
	require.Equal(t, read("../../../deploy/config/pd-epp-config.yaml"), read(filepath.Join(referenceConfigs, "pd-prefix-based.yaml")))
}

// TestBuiltInDefaultIsTheDefaultFile: a router given no configuration runs what default.yaml
// states, plugin for plugin, weight for weight and data source for data source.
func TestBuiltInDefaultIsTheDefaultFile(t *testing.T) {
	builtIn, builtInCfg, err := load(t, "")
	require.NoError(t, err)
	fromFile, fileCfg, err := load(t, readRef(t, "default.yaml"))
	require.NoError(t, err)
	require.Equal(t, effective(t, builtIn, builtInCfg), effective(t, fromFile, fileCfg))
	// The comparison can tell the two apart: a weight changed in the file shows.
	changed, changedCfg, err := load(t, strings.Replace(readRef(t, "default.yaml"), "weight: 3", "weight: 1", 1))
	require.NoError(t, err)
	require.NotEqual(t, effective(t, builtIn, builtInCfg), effective(t, changed, changedCfg))
}

// TestIndexKnobsMatchFiles: what policies.json states about each knob is what the files hold,
// and the router defaults it states for scorer weights and the picker are the router's own.
func TestIndexKnobsMatchFiles(t *testing.T) {
	idx := readIndex(t)
	for _, p := range idx.Policies {
		fs := files(p.Files)
		for _, k := range append(slices.Clone(p.Knobs), p.IneffectiveParameters...) {
			for shape, name := range fs {
				if k.AppliesTo != "" && k.AppliesTo != shape {
					continue
				}
				t.Run(p.Name+"/"+shape+"/"+k.ID, func(t *testing.T) {
					var doc struct {
						Plugins []struct {
							Type       string         `json:"type"`
							Name       string         `json:"name"`
							Parameters map[string]any `json:"parameters"`
						} `json:"plugins"`
						Profiles []struct {
							Name    string `json:"name"`
							Plugins []struct {
								PluginRef string   `json:"pluginRef"`
								Weight    *float64 `json:"weight"`
							} `json:"plugins"`
						} `json:"schedulingProfiles"`
					}
					require.NoError(t, yaml.Unmarshal([]byte(readRef(t, name)), &doc))
					switch k.Kind {
					case "scorer_weight":
						require.Equal(t, loader.DefaultScorerWeight, k.RouterDefault)
						seen := false
						for _, prof := range doc.Profiles {
							if k.Profile != "" && k.Profile != prof.Name {
								continue
							}
							for _, ref := range prof.Plugins {
								if ref.PluginRef != k.Plugin {
									continue
								}
								seen = true
								if k.InFile {
									require.NotNil(t, ref.Weight)
									require.EqualValues(t, k.FileValue, *ref.Weight)
								} else {
									require.Nil(t, ref.Weight, "index says the weight is not stated")
								}
							}
						}
						if k.InFile {
							require.True(t, seen, "no profile weights %s", k.Plugin)
						}
					case "plugin_parameter":
						if k.Parameter == "maxNumOfEndpoints" {
							require.EqualValues(t, picker.DefaultMaxNumOfEndpoints, k.RouterDefault)
						}
						var got any
						found := false
						for _, pl := range doc.Plugins {
							if pl.Type == k.Plugin {
								v, ok := pl.Parameters[k.Parameter]
								got, found = v, found || ok
							}
						}
						if k.InFile {
							require.True(t, found, "%s.%s is not in %s", k.Plugin, k.Parameter, name)
							require.EqualValues(t, k.FileValue, got)
						} else {
							require.False(t, found, "index says %s.%s is not stated in %s", k.Plugin, k.Parameter, name)
							require.Equal(t, "not stated", k.FileValue)
						}
					default:
						t.Fatalf("knob kind %q is not one the tests know", k.Kind)
					}
				})
			}
		}
	}
}

// TestRoundRobinOutsideHostingIsNotStrict: the round-robin file run through the router's
// scheduler without a hosted runtime, as the endpoint picker runs it, does not rotate the
// endpoints in a fixed order either. The picker's counter rotates a candidate list whose order
// is Go map order.
func TestRoundRobinOutsideHostingIsNotStrict(t *testing.T) {
	r, _, err := load(t, readRef(t, "round-robin.yaml"))
	require.NoError(t, err)
	sched := scheduling.NewSchedulerWithConfig(r.schedulerConfig)
	var candidates []fwksched.Endpoint
	for i := 0; i < 4; i++ {
		candidates = append(candidates, fwksched.NewEndpoint(
			&fwkdl.EndpointMetadata{ID: k8stypes.NamespacedName{Name: fmt.Sprintf("e%d", i)}}, &fwkdl.Metrics{}, fwkdl.NewAttributes()))
	}
	var seq []string
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		res, err := sched.Schedule(context.Background(), &fwksched.InferenceRequest{RequestID: fmt.Sprintf("r%d", i), TargetModel: "m"}, candidates)
		require.NoError(t, err)
		name := res.ProfileResults[res.PrimaryProfileName].TargetEndpoints[0].GetMetadata().ID.Name
		seq = append(seq, name)
		seen[name]++
	}
	require.Len(t, seen, 4, "an endpoint was never chosen: %v", seen)
	cycle := true
	for i := 1; i < len(seq); i++ {
		if seq[i] == seq[i-1] || seq[i] != seq[i%4] {
			cycle = false
		}
	}
	require.False(t, cycle, "the choice is a fixed cycle; the index says it is not")
}

// TestIndexSeparatesEffectiveFromIneffectiveParameters: the index lists a parameter as a knob only
// when it is claimed to change a hosted decision, and lists the ones that cannot with a reason;
// the decider of a P/D file is not a knob but names a complete variant. The hosted tests in
// pkg/embed (TestKnobMovesAHostedDecision, TestIneffectiveParametersLeaveTheDecisionAlone) hold
// the claims to the router.
func TestIndexSeparatesEffectiveFromIneffectiveParameters(t *testing.T) {
	idx := readIndex(t)
	byName := map[string]bool{}
	for _, p := range idx.Policies {
		byName[p.Name] = p.Files.PD != nil
	}
	for _, p := range idx.Policies {
		for _, k := range p.Knobs {
			require.NotNil(t, k.HostedEffect, "%s/%s has no hosted_effect", p.Name, k.ID)
			require.True(t, *k.HostedEffect, "%s/%s is listed as a knob but is marked ineffective", p.Name, k.ID)
			require.NotEqual(t, "plugin_choice", k.Kind, "%s/%s: a choice that needs more than one edit is not a knob", p.Name, k.ID)
			for _, r := range k.Requires {
				found := false
				for _, other := range p.Knobs {
					found = found || other.ID == r.ID
				}
				require.True(t, found, "%s/%s requires %s, which is not a knob of the policy", p.Name, k.ID, r.ID)
			}
		}
		for _, k := range p.IneffectiveParameters {
			require.NotNil(t, k.HostedEffect, "%s/%s has no hosted_effect", p.Name, k.ID)
			require.False(t, *k.HostedEffect, "%s/%s is listed as ineffective but marked effective", p.Name, k.ID)
			require.NotEmpty(t, k.Reason, "%s/%s is ineffective without a reason", p.Name, k.ID)
		}
		if p.PDDecider != nil {
			require.True(t, byName[p.PDDecider.CompleteVariant], "%s: complete_variant %q is not a P/D policy in the index", p.Name, p.PDDecider.CompleteVariant)
		}
	}
}
