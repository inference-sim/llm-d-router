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

package preciseprefixcache

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"runtime"
	"testing"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	"github.com/llm-d/llm-d-router/pkg/kvcache"
	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-router/pkg/kvevents"
	"github.com/llm-d/llm-d-router/test/utils"
)

// specGet reads a process producer's TTL-cache entry.
func specGet(p *Producer, key string) *ttlcache.Item[string, *speculativeEntries] {
	return p.speculativeCache.(*ttlcache.Cache[string, *speculativeEntries]).Get(key)
}

// fakeHostedRuntime is a single-goroutine hosted runtime: Go runs fn inline, Every records the
// loop without starting it, and Now is set by the test.
type fakeHostedRuntime struct {
	now    time.Time
	goes   int
	loops  []string
	locals map[any]any
}

func newFakeHostedRuntime() *fakeHostedRuntime {
	return &fakeHostedRuntime{now: time.Unix(1_000, 0), locals: map[any]any{}}
}

func (r *fakeHostedRuntime) Now() time.Time           { return r.now }
func (r *fakeHostedRuntime) Clock() clock.WithTicker  { return testingclock.NewFakeClock(r.now) }
func (r *fakeHostedRuntime) Go(fn func())             { r.goes++; fn() }
func (r *fakeHostedRuntime) Park(func() bool)         {}
func (r *fakeHostedRuntime) Rand(string) hosting.Rand { return rand.New(rand.NewPCG(1, 2)) } //nolint:gosec // test
func (r *fakeHostedRuntime) Hosted() bool             { return true }
func (r *fakeHostedRuntime) advance(d time.Duration)  { r.now = r.now.Add(d) }
func (r *fakeHostedRuntime) Every(_ context.Context, name string, _ time.Duration, _ func(context.Context)) {
	r.loops = append(r.loops, name)
}

func (r *fakeHostedRuntime) WithTimeout(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

func (r *fakeHostedRuntime) WithDeadlineCause(parent context.Context, _ time.Time, _ error) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

func (r *fakeHostedRuntime) Local(key any, init func() any) any {
	if v, ok := r.locals[key]; ok {
		return v
	}
	v := init()
	r.locals[key] = v
	return v
}

const (
	hostedModel = "hosted-model"
	podA        = "10.0.0.1:8080"
	podB        = "10.0.0.2:8080"
)

// hostedConfig is a configuration that, in a process, would start every background loop the
// producer has: metrics logging, workers, a local ZMQ subscriber, pod discovery and the TTL cache.
func hostedConfig(t *testing.T, speculative bool) PluginConfig {
	t.Helper()
	idxCfg, err := kvcache.NewDefaultConfig()
	require.NoError(t, err)
	idxCfg.KVBlockIndexConfig.EnableMetrics = true
	idxCfg.KVBlockIndexConfig.MetricsLoggingInterval = time.Second
	events := kvevents.DefaultConfig()
	events.ZMQEndpoint = "tcp://127.0.0.1:1"
	return PluginConfig{
		IndexerConfig:       idxCfg,
		KVEventsConfig:      events,
		SpeculativeIndexing: speculative,
		SpeculativeTTL:      "2s",
	}
}

func newHostedTestProducer(t *testing.T, speculative bool) (*Producer, *fakeHostedRuntime, context.Context) {
	t.Helper()
	rt := newFakeHostedRuntime()
	ctx := hosting.Into(utils.NewTestContext(t), rt)
	p, err := New(ctx, "hosted", hostedConfig(t, speculative))
	require.NoError(t, err)
	return p, rt, ctx
}

// promptTokens returns n blocks' worth of distinct tokens at the default block size.
func promptTokens(blocks int) []uint32 {
	tokens := make([]uint32, blocks*testBlockSize)
	for i := range tokens {
		tokens[i] = uint32(i + 7)
	}
	return tokens
}

func hostedRequest(id string, tokens []uint32) *scheduling.InferenceRequest {
	return &scheduling.InferenceRequest{
		RequestID:   id,
		TargetModel: hostedModel,
		Body: &fwkrh.InferenceRequestBody{
			TokenizedRequest: &fwkrh.TokenizedRequest{Prompts: []fwkrh.PromptTokens{{TokenIDs: tokens}}},
		},
	}
}

// produceMatch runs Produce for tokens over both test pods and returns each pod's cached-block
// count.
func produceMatch(t *testing.T, ctx context.Context, p *Producer, id string, tokens []uint32) map[string]int {
	t.Helper()
	endpoints := freshEndpoints()
	require.NoError(t, p.Produce(ctx, hostedRequest(id, tokens), endpoints))
	out := map[string]int{}
	for _, ep := range endpoints {
		raw, ok := ep.Get(p.dk)
		require.True(t, ok)
		info := raw.(*attrprefix.PrefixCacheMatchInfo)
		md := ep.GetMetadata()
		out[md.Address+":"+md.Port] = info.CachedBlockCount()
	}
	return out
}

func storedBatch(tokens []uint32, hashes ...uint64) *kvevents.EventBatch {
	return &kvevents.EventBatch{Events: []kvevents.GenericEvent{
		&kvevents.BlockStoredEvent{BlockHashes: hashes, Tokens: tokens, BlockSize: testBlockSize},
	}}
}

// (a) Built hosted, the producer starts no goroutine and no runtime loop of its own (the plugin
// state's cleanup loop is the runtime's, not the producer's), even with a configuration that starts
// metrics logging, workers, a ZMQ subscriber and the TTL cache in a process.
func TestHostedNew_StartsNoBackgroundWork(t *testing.T) {
	before := runtime.NumGoroutine()
	p, rt, _ := newHostedTestProducer(t, true)
	after := runtime.NumGoroutine()

	assert.LessOrEqual(t, after, before, "hosted New started goroutines")
	assert.Zero(t, rt.goes, "hosted New must not start work through rt.Go")
	assert.Equal(t, []string{"plugin-state-cleanup"}, rt.loops)
	_, isHostedStore := p.speculativeCache.(*hostedSpeculativeStore)
	assert.True(t, isHostedStore, "speculative entries must expire on the runtime clock")
	require.NotNil(t, p.hostedPool(), "hosted New must build a hosted pool (no queues, no workers)")
	ids, _ := p.subscribersManager.GetActiveSubscribers()
	assert.Empty(t, ids, "no ZMQ subscriber")
}

// Discovery subscribes nothing under a hosted runtime.
func TestHostedExtract_SubscribesNothing(t *testing.T) {
	p, rt, ctx := newHostedTestProducer(t, false)
	ep := newEndpoint("pod-a", "10.0.0.1")
	require.NoError(t, p.Extract(ctx, fwkdl.EndpointEvent{Type: fwkdl.EventAddOrUpdate, Endpoint: ep}))
	require.NoError(t, p.Extract(ctx, fwkdl.EndpointEvent{Type: fwkdl.EventDelete, Endpoint: ep}))
	ids, _ := p.subscribersManager.GetActiveSubscribers()
	assert.Empty(t, ids)
	assert.Zero(t, rt.goes)
}

// Hosted index backends are limited to the in-memory index.
func TestHostedNew_RefusesBackgroundIndexes(t *testing.T) {
	ctx := hosting.Into(utils.NewTestContext(t), newFakeHostedRuntime())
	for name, mutate := range map[string]func(*kvblock.IndexConfig){
		"redis":      func(c *kvblock.IndexConfig) { c.RedisConfig = &kvblock.RedisIndexConfig{} },
		"cost-aware": func(c *kvblock.IndexConfig) { c.CostAwareMemoryConfig = &kvblock.CostAwareMemoryIndexConfig{} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := hostedConfig(t, false)
			mutate(cfg.IndexerConfig.KVBlockIndexConfig)
			_, err := New(ctx, "hosted", cfg)
			require.Error(t, err)
		})
	}
	cfg := hostedConfig(t, false)
	cfg.KVEventsConfig.Concurrency = 0
	_, err := New(ctx, "hosted", cfg)
	require.Error(t, err, "concurrency is validated as in a process")
}

// (b) Ingested stores make Produce report the pod's prefix match; removes shorten it; an
// AllBlocksCleared event and HostedClearPod clear it. HostedPodBlocks follows the same changes.
func TestHostedIngest_DrivesProduce(t *testing.T) {
	p, _, ctx := newHostedTestProducer(t, false)
	tokens := promptTokens(3)

	assert.Equal(t, map[string]int{podA: 0, podB: 0}, produceMatch(t, ctx, p, "r0", tokens))

	require.NoError(t, p.HostedIngest(ctx, podA, hostedModel, storedBatch(tokens, 101, 102, 103)))
	assert.Equal(t, map[string]int{podA: 3, podB: 0}, produceMatch(t, ctx, p, "r1", tokens))
	blocks, err := p.HostedPodBlocks(podA)
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{101: true, 102: true, 103: true}, blocks)
	blocks, err = p.HostedPodBlocks(podB)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	removed := &kvevents.EventBatch{Events: []kvevents.GenericEvent{&kvevents.BlockRemovedEvent{BlockHashes: []uint64{102}}}}
	require.NoError(t, p.HostedIngest(ctx, podA, hostedModel, removed))
	assert.Equal(t, map[string]int{podA: 1, podB: 0}, produceMatch(t, ctx, p, "r2", tokens))
	blocks, err = p.HostedPodBlocks(podA)
	require.NoError(t, err)
	assert.Equal(t, map[uint64]bool{101: true, 103: true}, blocks)

	cleared := &kvevents.EventBatch{Events: []kvevents.GenericEvent{&kvevents.AllBlocksClearedEvent{}}}
	require.NoError(t, p.HostedIngest(ctx, podA, hostedModel, cleared))
	assert.Equal(t, map[string]int{podA: 0, podB: 0}, produceMatch(t, ctx, p, "r3", tokens))

	require.NoError(t, p.HostedIngest(ctx, podB, hostedModel, storedBatch(tokens, 201, 202, 203)))
	assert.Equal(t, map[string]int{podA: 0, podB: 3}, produceMatch(t, ctx, p, "r4", tokens))
	require.NoError(t, p.HostedClearPod(ctx, podB))
	assert.Equal(t, map[string]int{podA: 0, podB: 0}, produceMatch(t, ctx, p, "r5", tokens))
	blocks, err = p.HostedPodBlocks(podB)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// (c) A speculative entry seeded by PreRequest counts toward the next Produce until the runtime
// clock passes its TTL, and not a nanosecond less; HostedPodBlocks never reports it.
func TestHostedSpeculative_ExpiresOnRuntimeClock(t *testing.T) {
	p, rt, ctx := newHostedTestProducer(t, true)
	tokens := promptTokens(2)

	endpoints := freshEndpoints()
	req := hostedRequest("seed", tokens)
	require.NoError(t, p.Produce(ctx, req, endpoints))
	require.NoError(t, p.PreRequest(ctx, req, primaryOnly("default", endpoints[0])))
	assert.Equal(t, []string{"seed"}, p.speculativeCache.Keys())

	blocks, err := p.HostedPodBlocks(podA)
	require.NoError(t, err)
	assert.Empty(t, blocks, "speculative entries carry no engine keys")

	rt.advance(2 * time.Second) // exactly the TTL: not yet passed
	assert.Equal(t, map[string]int{podA: 2, podB: 0}, produceMatch(t, ctx, p, "probe-1", tokens))
	assert.Equal(t, []string{"seed"}, p.speculativeCache.Keys())

	rt.advance(time.Nanosecond)
	assert.Equal(t, map[string]int{podA: 0, podB: 0}, produceMatch(t, ctx, p, "probe-2", tokens))
	assert.Empty(t, p.speculativeCache.Keys())

	payload, err := p.DumpState()
	require.NoError(t, err)
	var state precisePrefixState
	require.NoError(t, json.Unmarshal(payload, &state))
	assert.Empty(t, state.SpeculativeEntries)
	assert.Zero(t, rt.goes)
}

// Expired entries are evicted in (expiry, insertion) order; a replaced entry is not evicted.
func TestHostedSpeculativeStore_EvictionOrder(t *testing.T) {
	rt := newFakeHostedRuntime()
	var evicted []kvblock.BlockHash
	idx := &fakeKVBlockIndex{evictFn: func(_ context.Context, keyType kvblock.KeyType, keys []kvblock.BlockHash, _ []kvblock.PodEntry) error {
		assert.Equal(t, kvblock.RequestKey, keyType)
		evicted = append(evicted, keys...)
		return nil
	}}
	s := newHostedSpeculativeStore(context.Background(), rt, idx, time.Second)
	entry := func(key kvblock.BlockHash) *speculativeEntries {
		return &speculativeEntries{
			perPromptKeys: [][]kvblock.BlockHash{{key}},
			podEntries:    []kvblock.PodEntry{{PodIdentifier: podA, Speculative: true}},
		}
	}
	s.Set("late", entry(3), 2*time.Second)
	s.Set("b", entry(2), ttlcache.DefaultTTL)
	s.Set("a", entry(1), time.Second)
	s.Set("replaced", entry(9), time.Second)
	s.Set("replaced", entry(4), 3*time.Second)
	s.Set("forever", entry(5), ttlcache.NoTTL)

	rt.advance(time.Second)
	s.expire()
	assert.Empty(t, evicted)

	rt.advance(5 * time.Second)
	s.expire()
	assert.Equal(t, []kvblock.BlockHash{2, 1, 3, 4}, evicted)
	assert.Equal(t, []string{"forever"}, s.Keys())
}

// (d) Without a hosted runtime New takes the process path, and the Hosted* entry points refuse.
func TestRealNew_ProcessPath(t *testing.T) {
	ctx, cancel := context.WithCancel(utils.NewTestContext(t))
	t.Cleanup(cancel)
	idxCfg, err := kvcache.NewDefaultConfig()
	require.NoError(t, err)

	p, err := New(ctx, "real", PluginConfig{IndexerConfig: idxCfg, SpeculativeIndexing: true})
	require.NoError(t, err)
	_, isTTLCache := p.speculativeCache.(*ttlcache.Cache[string, *speculativeEntries])
	assert.True(t, isTTLCache)
	_, isSubscriberManager := p.subscribersManager.(*kvevents.SubscriberManager)
	assert.True(t, isSubscriberManager)
	require.ErrorIs(t, p.HostedIngest(ctx, podA, hostedModel, storedBatch(promptTokens(1), 1)), errNotHosted)
	require.ErrorIs(t, p.HostedClearPod(ctx, podA), errNotHosted)
	_, err = p.HostedPodBlocks(podA)
	require.ErrorIs(t, err, errNotHosted)
	p.subscribersManager.Shutdown(ctx)
}

// With speculative indexing off, both paths leave a true nil store, so DumpState does not call
// through a typed nil.
func TestSpeculativeDisabled_NilStore(t *testing.T) {
	ctx, cancel := context.WithCancel(utils.NewTestContext(t))
	t.Cleanup(cancel)
	idxCfg, err := kvcache.NewDefaultConfig()
	require.NoError(t, err)
	proc, err := New(ctx, "real", PluginConfig{IndexerConfig: idxCfg})
	require.NoError(t, err)
	defer proc.subscribersManager.Shutdown(ctx)
	hosted, _, _ := newHostedTestProducer(t, false)

	for _, p := range []*Producer{proc, hosted} {
		assert.Nil(t, p.speculativeCache)
		_, err := p.DumpState()
		require.NoError(t, err)
	}
}

// Hosted Produce records each candidate's prediction (by endpoint ID) and their maximum in the
// plugin state PreRequest reads, exactly as in a process (#3281), and speculative indexing does not
// disturb it.
func TestHostedProduce_RecordsPerCandidatePredictions(t *testing.T) {
	p, _, ctx := newHostedTestProducer(t, true)
	tokens := promptTokens(3)
	require.NoError(t, p.HostedIngest(ctx, podA, hostedModel, storedBatch(tokens, 101, 102, 103)))

	endpoints := freshEndpoints()
	req := hostedRequest("r-state", tokens)
	require.NoError(t, p.Produce(ctx, req, endpoints))

	state, err := plugin.ReadPluginStateKey[*bestAvailableState](p.pluginState, req.RequestID, bestAvailableStateKey)
	require.NoError(t, err)
	want := map[fwkdl.ID]int{}
	for _, ep := range endpoints {
		md := ep.GetMetadata()
		want[md.ID] = 0
		if md.Address+":"+md.Port == podA {
			want[md.ID] = 3 * testBlockSize
		}
	}
	assert.Equal(t, want, state.predictedCachedTokens)
	assert.Equal(t, 3*testBlockSize, state.cachedTokens)

	require.NoError(t, p.PreRequest(ctx, req, primaryOnly("default", endpoints[0])))
	_, err = plugin.ReadPluginStateKey[*bestAvailableState](p.pluginState, req.RequestID, bestAvailableStateKey)
	assert.Error(t, err, "PreRequest deletes the request's state")
}
