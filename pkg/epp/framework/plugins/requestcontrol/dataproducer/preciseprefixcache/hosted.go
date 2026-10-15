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
	"container/heap"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/jellydator/ttlcache/v3"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	attrprefix "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/datalayer/attribute/prefix"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/prefixmetrics"
	"github.com/llm-d/llm-d-router/pkg/kvcache"
	"github.com/llm-d/llm-d-router/pkg/kvcache/kvblock"
	"github.com/llm-d/llm-d-router/pkg/kvevents"
	"github.com/llm-d/llm-d-router/pkg/kvevents/engineadapter"
)

// speculativeStore holds the speculative entries PreRequest seeds until their TTL passes. A
// process router uses a *ttlcache.Cache, which expires entries on the wall clock from its own
// goroutine; a hosted router uses a hostedSpeculativeStore, which expires them on the runtime clock
// when the producer next reads the index.
type speculativeStore interface {
	Set(key string, value *speculativeEntries, ttl time.Duration) *ttlcache.Item[string, *speculativeEntries]
	Keys() []string
}

var (
	_ speculativeStore  = (*ttlcache.Cache[string, *speculativeEntries])(nil)
	_ speculativeStore  = (*hostedSpeculativeStore)(nil)
	_ subscriberManager = (*hostedEvents)(nil)
)

// errNotHosted is returned by the Hosted* methods of a producer built without a hosted runtime.
var errNotHosted = errors.New("precise-prefix-cache-producer: not built under a hosted runtime")

// newHostedProducer is New under a hosted runtime (hosting.From(ctx).Hosted()). It builds the same
// index, token processor and event handling as New but starts nothing: no indexer goroutine, no
// metrics-logging loop, no event workers or queues, and no ZMQ subscriber. The host delivers KV
// events with HostedIngest and HostedClearPod; endpoint discovery (Extract) subscribes nothing.
// Speculative entries expire on the runtime clock, swept before each index read.
//
// Index backends that run their own goroutines or reach the network (Redis, cost-aware memory)
// are refused.
func newHostedProducer(ctx context.Context, name string, config PluginConfig) (*Producer, error) {
	rt := hosting.From(ctx)
	if config.TokenProcessorConfig == nil {
		config.TokenProcessorConfig = kvblock.DefaultTokenProcessorConfig()
	}
	if config.KVEventsConfig == nil {
		config.KVEventsConfig = kvevents.DefaultConfig()
	}
	if config.KVEventsConfig.Concurrency <= 0 {
		// Refused as NewPool refuses it, so a configuration is accepted hosted iff it is accepted
		// in a process, although no worker runs.
		return nil, fmt.Errorf("failed to create KV-events pool: kvEventsConfig.concurrency must be positive, got %d",
			config.KVEventsConfig.Concurrency)
	}

	var podSelector labels.Selector
	if kc := config.KVEventsConfig; kc.DiscoverPods && kc.PodDiscoveryConfig != nil && kc.PodDiscoveryConfig.PodLabelSelector != "" {
		sel, err := labels.Parse(kc.PodDiscoveryConfig.PodLabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid kvEventsConfig.podDiscoveryConfig.podLabelSelector %q: %w",
				kc.PodDiscoveryConfig.PodLabelSelector, err)
		}
		podSelector = sel
	}

	indexerConfig, err := hostedIndexerConfig(config.IndexerConfig)
	if err != nil {
		return nil, err
	}

	tokenProcessor, err := kvblock.NewChunkedTokenDatabase(config.TokenProcessorConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create token processor: %w", err)
	}

	indexer, err := kvcache.NewKVCacheIndexer(ctx, indexerConfig, tokenProcessor)
	if err != nil {
		return nil, fmt.Errorf("failed to create kvcache.Indexer: %w", err)
	}
	// indexer.Run only waits for ctx; a hosted router does not start it.

	adapter, err := engineadapter.NewAdapter(config.KVEventsConfig.EngineType)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV-events engine adapter: %w", err)
	}
	pool, err := kvevents.NewHostedPool(config.KVEventsConfig, indexer.KVBlockIndex(), tokenProcessor, adapter)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV-events pool: %w", err)
	}

	var speculative speculativeStore // a true nil interface when speculative indexing is off
	var speculativeTTL time.Duration
	if config.SpeculativeIndexing {
		speculativeTTL, err = parseSpeculativeTTL(config.SpeculativeTTL)
		if err != nil {
			return nil, err
		}
		speculative = newHostedSpeculativeStore(ctx, rt, indexer.KVBlockIndex(), speculativeTTL)
	}

	prefixmetrics.Register()

	return &Producer{
		typedName:          plugin.TypedName{Type: PluginType, Name: name},
		kvCacheIndexer:     indexer,
		subscribersManager: &hostedEvents{pool: pool},
		kvEventsConfig:     config.KVEventsConfig,
		podSelector:        podSelector,
		dk:                 attrprefix.PrefixCacheMatchInfoDataKey.WithNonEmptyProducerName(name),
		pluginState:        plugin.NewPluginState(ctx),
		speculativeCache:   speculative,
		speculativeTTL:     speculativeTTL,
		speculativeEnabled: config.SpeculativeIndexing,
		blockSizeTokens:    tokenProcessor.BlockSize(),
		subscriberCtx:      ctx,
	}, nil
}

// hostedIndexerConfig returns a copy of cfg fit for a hosted router: the metrics-logging loop is
// switched off (metrics are still recorded when enabled), and backends that run their own
// goroutines or reach the network are refused.
func hostedIndexerConfig(cfg *kvcache.Config) (*kvcache.Config, error) {
	if cfg == nil {
		return nil, errors.New("failed to create kvcache.Indexer: config cannot be nil")
	}
	out := *cfg
	if kb := cfg.KVBlockIndexConfig; kb != nil {
		switch {
		case kb.CostAwareMemoryConfig != nil:
			return nil, errors.New("a hosted precise-prefix-cache-producer supports only the in-memory index; " +
				"costAwareMemoryConfig is set")
		case kb.RedisConfig != nil:
			return nil, errors.New("a hosted precise-prefix-cache-producer supports only the in-memory index; " +
				"redisConfig is set")
		}
		indexConfig := *kb
		indexConfig.MetricsLoggingInterval = 0
		out.KVBlockIndexConfig = &indexConfig
	}
	return &out, nil
}

// parseSpeculativeTTL resolves PluginConfig.SpeculativeTTL as buildSpeculativeCache does.
func parseSpeculativeTTL(raw string) (time.Duration, error) {
	ttl := defaultSpeculativeTTL
	if raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("invalid speculativeTTL %q: %w", raw, err)
		}
		if parsed > 0 {
			ttl = parsed
		}
	}
	return ttl, nil
}

// hostedEvents is the hosted producer's subscriberManager. It subscribes nothing: the host
// delivers KV events through the producer's Hosted* methods, which apply them to pool.
type hostedEvents struct {
	pool *kvevents.Pool
}

func (*hostedEvents) EnsureSubscriber(context.Context, string, string, string, string, string, string, bool) error {
	return nil
}

func (*hostedEvents) RemoveSubscriber(context.Context, string) bool { return false }

func (*hostedEvents) GetActiveSubscribers() ([]string, []string) { return nil, nil }

func (*hostedEvents) Shutdown(context.Context) {}

// hostedPool returns the pool a hosted producer applies events to, or nil for a process producer.
func (p *Producer) hostedPool() *kvevents.Pool {
	if events, ok := p.subscribersManager.(*hostedEvents); ok {
		return events.pool
	}
	return nil
}

// HostedIngest applies one batch of KV events that the engine serving podIdentifier
// ("address:port", the identity Produce matches endpoints by) published for modelName. Batches of
// one pod must arrive in emission order. Speculative entries due to expire by the runtime clock are
// evicted first. It returns an error on a producer not built under a hosted runtime.
func (p *Producer) HostedIngest(ctx context.Context, podIdentifier, modelName string, batch *kvevents.EventBatch) error {
	pool := p.hostedPool()
	if pool == nil {
		return errNotHosted
	}
	p.expireSpeculative()
	return pool.ProcessEventBatch(ctx, batch, podIdentifier, modelName)
}

// HostedClearPod drops every index entry of podIdentifier across all device tiers, as an
// AllBlocksCleared event from that pod does; a host calls it when the pod's engine resets its cache
// or the pod goes away. It returns an error on a producer not built under a hosted runtime.
func (p *Producer) HostedClearPod(ctx context.Context, podIdentifier string) error {
	pool := p.hostedPool()
	if pool == nil {
		return errNotHosted
	}
	p.expireSpeculative()
	return pool.ClearPod(ctx, podIdentifier)
}

// HostedPodBlocks returns the engine block keys (the BlockHashes of ingested BlockStored events)
// the index holds for podIdentifier on any device tier, after evicting speculative entries due to
// expire. Speculative entries are not included. It reads without refreshing index recency, so it
// does not perturb later decisions. It returns an error on a producer not built under a hosted
// runtime or whose index cannot be enumerated.
func (p *Producer) HostedPodBlocks(podIdentifier string) (map[uint64]bool, error) {
	if p.hostedPool() == nil {
		return nil, errNotHosted
	}
	p.expireSpeculative()
	return kvblock.PodEngineKeys(p.kvCacheIndexer.KVBlockIndex(), podIdentifier)
}

// expireSpeculative evicts the hosted speculative entries whose TTL has passed. A process producer
// expires them from the TTL cache's own goroutine instead, so this is a no-op there.
func (p *Producer) expireSpeculative() {
	if store, ok := p.speculativeCache.(*hostedSpeculativeStore); ok {
		store.expire()
	}
}

// hostedSpeculativeStore is the speculative-entry TTL store of a hosted producer. An entry set at
// runtime time t with ttl d expires once the runtime clock passes t+d (strictly after, as the TTL
// cache judges expiry). Expiry is lazy: expire, called before every index read, evicts the expired
// entries in (expiry, insertion) order, so a host replaying the same schedule gets the same
// evictions in the same order. No timer runs.
type hostedSpeculativeStore struct {
	mu     sync.Mutex
	rt     hosting.Runtime
	ctx    context.Context
	index  kvblock.Index
	logger logr.Logger
	ttl    time.Duration

	seq     uint64
	live    map[string]*hostedSpeculativeItem // key -> its current item
	queue   hostedExpiryQueue                 // items that expire, earliest first
	forever []*hostedSpeculativeItem          // items set with ttlcache.NoTTL
}

type hostedSpeculativeItem struct {
	key       string
	value     *speculativeEntries
	expiresAt time.Time
	seq       uint64
}

func newHostedSpeculativeStore(ctx context.Context, rt hosting.Runtime, index kvblock.Index,
	ttl time.Duration,
) *hostedSpeculativeStore {
	return &hostedSpeculativeStore{
		rt:     rt,
		ctx:    ctx,
		index:  index,
		logger: log.FromContext(ctx).WithName(PluginType),
		ttl:    ttl,
		live:   map[string]*hostedSpeculativeItem{},
	}
}

// Set stores value under key, replacing any earlier value without evicting it (as the TTL cache
// does). ttl follows the TTL cache's conventions: ttlcache.DefaultTTL selects the store's TTL and
// ttlcache.NoTTL never expires. The TTL cache's item is not modelled, so Set returns nil.
func (s *hostedSpeculativeStore) Set(key string, value *speculativeEntries, ttl time.Duration,
) *ttlcache.Item[string, *speculativeEntries] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ttl == ttlcache.DefaultTTL {
		ttl = s.ttl
	}
	s.seq++
	item := &hostedSpeculativeItem{key: key, value: value, seq: s.seq}
	if ttl == ttlcache.NoTTL {
		s.forever = append(s.forever, item)
	} else {
		item.expiresAt = s.rt.Now().Add(ttl)
		heap.Push(&s.queue, item)
	}
	s.live[key] = item // a replaced item stays queued and is skipped when popped
	return nil
}

// Keys returns the keys of the entries that have not expired, sorted.
func (s *hostedSpeculativeStore) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.rt.Now()
	keys := make([]string, 0, len(s.live))
	for _, item := range s.forever {
		if s.live[item.key] == item {
			keys = append(keys, item.key)
		}
	}
	for _, item := range s.queue {
		if s.live[item.key] == item && !item.expiresAt.Before(now) {
			keys = append(keys, item.key)
		}
	}
	slices.Sort(keys)
	return keys
}

// expire evicts every entry whose expiry the runtime clock has passed, earliest first.
func (s *hostedSpeculativeStore) expire() {
	now := s.rt.Now()
	var expired []*hostedSpeculativeItem
	s.mu.Lock()
	for s.queue.Len() > 0 && s.queue[0].expiresAt.Before(now) {
		item := heap.Pop(&s.queue).(*hostedSpeculativeItem)
		if s.live[item.key] != item {
			continue // replaced by a later Set
		}
		delete(s.live, item.key)
		expired = append(expired, item)
	}
	s.mu.Unlock()
	for _, item := range expired {
		s.evict(item)
	}
}

// evict rolls back an expired entry's speculative index rows, as the TTL cache's eviction callback
// in buildSpeculativeCache does.
func (s *hostedSpeculativeStore) evict(item *hostedSpeculativeItem) {
	entries := item.value
	if entries == nil {
		return
	}
	keys := make([]kvblock.BlockHash, 0)
	for _, promptKeys := range entries.perPromptKeys {
		keys = append(keys, promptKeys...)
	}
	if len(keys) == 0 || len(entries.podEntries) == 0 {
		return
	}
	if err := s.index.Evict(s.ctx, kvblock.RequestKey, keys, entries.podEntries); err != nil {
		s.logger.Error(err, "Failed to evict speculative entries on TTL expiry",
			"requestID", item.key, "keys", len(keys))
	}
}

// hostedExpiryQueue is a min-heap of items ordered by expiry, then insertion sequence.
type hostedExpiryQueue []*hostedSpeculativeItem

func (q hostedExpiryQueue) Len() int { return len(q) }

func (q hostedExpiryQueue) Less(i, j int) bool {
	if !q[i].expiresAt.Equal(q[j].expiresAt) {
		return q[i].expiresAt.Before(q[j].expiresAt)
	}
	return q[i].seq < q[j].seq
}

func (q hostedExpiryQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *hostedExpiryQueue) Push(x any) { *q = append(*q, x.(*hostedSpeculativeItem)) }

func (q *hostedExpiryQueue) Pop() any {
	old := *q
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return item
}
