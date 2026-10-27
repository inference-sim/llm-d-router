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
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	contract "github.com/inference-sim/blis-schemas/embed"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	giev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	"github.com/llm-d/llm-d-router/apix/v1alpha2"
	"github.com/llm-d/llm-d-router/cmd/epp/runner"
	"github.com/llm-d/llm-d-router/pkg/common"
	"github.com/llm-d/llm-d-router/pkg/common/hosting"
	"github.com/llm-d/llm-d-router/pkg/epp/controller"
	"github.com/llm-d/llm-d-router/pkg/epp/datalayer"
	fcregistry "github.com/llm-d/llm-d-router/pkg/epp/flowcontrol/registry"
	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/preciseprefixcache"
	"github.com/llm-d/llm-d-router/pkg/epp/handlers"
	runserver "github.com/llm-d/llm-d-router/pkg/epp/server"
	poolutil "github.com/llm-d/llm-d-router/pkg/epp/util/pool"
)

var (
	_ contract.Constructor      = New
	_ contract.Router           = (*Router)(nil)
	_ contract.KVIndexInspector = (*Router)(nil)
)

// embedMu serializes router construction: the router registers its plugin factories and
// feature gates in process-wide registries.
var embedMu sync.Mutex

// defaultNamespace is the namespace of a pool whose flags name none.
const defaultNamespace = "default"

// Router is a router embedded through the blis-schemas embed contract. It is not safe for
// concurrent use: the host calls it from one goroutine at a time (the contract's call rule).
type Router struct {
	ctx      context.Context // the router's lifetime; carries the hosting runtime and the logger
	cancel   context.CancelFunc
	stopping func() // starts the hosting runtime's shutdown order

	requirements contract.Requirements
	pool         contract.Pool
	flowControl  contract.FlowControl
	idleSkip     bool

	emb      *runner.Embedded
	server   *handlers.StreamingServer
	kube     client.Client
	pods     *controller.PodReconciler
	kvIngest []*preciseprefixcache.Producer // in name order

	endpoints map[string]*corev1.Pod
	flights   map[string]*flight // undecided and dispatched requests, by request ID
	stopped   bool
}

// New builds a router on rt from cfg: the router's own startup path (runner.Embed) over the
// deployment's EPP flags, EndpointPickerConfig and manifests. It is a blis-schemas
// embed.Constructor. Its steps, each of which may refuse the configuration:
//
//  1. Parse the EPP flags with the router's flag set ("EPP flags: ...") and read the
//     configuration they name ("EPP flag --config-file: ...").
//  2. Parse the manifests ("manifest document N: ...") and resolve the pool: the InferencePool
//     --pool-name names, or the standalone pool of --endpoint-selector.
//  3. Call cfg.Check with the Requirements and Pool, and return its error as is.
//  4. Refuse configurations the hosting cannot run: a topology form that needs a coordinator, a
//     render backend that discovers endpoints, a Redis or cost-aware KV index
//     ("the router configuration cannot be simulated faithfully: ...").
//  5. Assemble the router ("llm-d router: ..."). Each plugin type is refused before its
//     constructor runs, with an error wrapping an *embed.RefusalError: a type the fork cannot
//     host with its reason; otherwise a type cfg.Allow refuses with Allow's reason; otherwise a
//     type the fork has not audited.
//  6. Check every plugin the assembled router holds, by any path, against the same policy.
//  7. Reconcile the manifests' InferenceObjectives and InferenceModelRewrites.
//
// New draws no randomness. A failed New has stopped everything it started.
func New(ctx context.Context, rt contract.Runtime, cfg contract.Config) (contract.Router, error) {
	flags, err := parseFlags(cfg.Flags)
	if err != nil {
		return nil, err
	}
	raw, err := configBytes(flags.opts, cfg.Config)
	if err != nil {
		return nil, err
	}
	doc := readConfig(raw)
	scheme := newScheme()
	res, err := parseManifests(scheme, cfg.Manifests, poolNamespace(flags.opts))
	if err != nil {
		return nil, err
	}
	pool, gknn, err := resolvePool(flags.opts, res)
	if err != nil {
		return nil, err
	}
	r := &Router{
		requirements: requirements(flags, doc),
		pool: contract.Pool{Namespace: gknn.Namespace, Name: gknn.Name, Group: gknn.Group, Kind: gknn.Kind,
			TargetPorts: slices.Clone(pool.TargetPorts)},
		endpoints: map[string]*corev1.Pod{},
		flights:   map[string]*flight{},
	}
	if cfg.Check != nil {
		if err := cfg.Check(r.Requirements(), r.Pool()); err != nil {
			return nil, err
		}
	}
	if err := hostingRefusal(doc); err != nil {
		return nil, errors.New(refusedPrefix + err.Error())
	}

	logger := logr.Discard()
	if cfg.Logger != nil {
		logger = logr.FromSlogHandler(cfg.Logger.Handler())
	}
	hrt, stopping := adaptRuntime(rt)
	r.stopping = stopping
	r.ctx, r.cancel = context.WithCancel(hosting.Into(log.IntoContext(ctx, logger), hrt))
	if err := r.build(flags.opts, pool, gknn, res, scheme, policy{allow: cfg.Allow}, logger); err != nil {
		r.Stop()
		return nil, err
	}
	return r, nil
}

func (r *Router) build(opts *runserver.Options, pool *datalayer.EndpointPool, gknn common.GKNN, res *resources,
	scheme *runtime.Scheme, p policy, logger logr.Logger) error {
	embedMu.Lock()
	ctrl.SetLogger(logger)
	var restore func()
	emb, err := runner.Embed(r.ctx, runner.EmbedOptions{Options: opts, Pool: pool, AfterRegister: func() error {
		restore = p.refuseFactories()
		return nil
	}})
	if restore != nil {
		restore()
	}
	embedMu.Unlock()
	if err != nil {
		return fmt.Errorf("llm-d router: %w", err)
	}
	r.emb = emb
	if err := p.validateEmbedded(emb.Config, emb.Handle); err != nil {
		return err
	}
	r.kvIngest = kvIngesters(emb.Handle)
	r.flowControl = flowControl(emb)
	r.idleSkip = idleSkippable(emb)
	r.server = handlers.NewStreamingServer(emb.Datastore, emb.Director, emb.Parsers, 0)
	r.server.SetResponders(nil)

	objs := res.objects()
	r.kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r.pods = &controller.PodReconciler{Reader: r.kube, Datastore: emb.Datastore}
	if opts.PoolName != "" {
		return r.reconcileResources(gknn, res, objs)
	}
	return nil
}

func poolNamespace(o *runserver.Options) string {
	if o.PoolNamespace != "" {
		return o.PoolNamespace
	}
	return defaultNamespace
}

// resolvePool finds the pool the router serves: the InferencePool named by --pool-name, or the
// standalone pool --endpoint-selector describes.
func resolvePool(o *runserver.Options, res *resources) (*datalayer.EndpointPool, common.GKNN, error) {
	ns := poolNamespace(o)
	if o.PoolName != "" {
		for _, p := range res.pools {
			if p.Name == o.PoolName && p.Namespace == ns {
				if len(res.pools) > 1 {
					return nil, common.GKNN{}, fmt.Errorf("the manifests define %d InferencePools; a simulation runs one router for one pool", len(res.pools))
				}
				gknn := common.GKNN{
					NamespacedName: types.NamespacedName{Namespace: ns, Name: o.PoolName},
					GroupKind:      schema.GroupKind{Group: o.PoolGroup, Kind: "InferencePool"},
				}
				return poolutil.InferencePoolToEndpointPool(p), gknn, nil
			}
		}
		return nil, common.GKNN{}, fmt.Errorf("--pool-name=%s: no InferencePool %s/%s in the manifests", o.PoolName, ns, o.PoolName)
	}
	if len(res.pools) > 0 {
		return nil, common.GKNN{}, errors.New("the manifests define an InferencePool but the EPP flags select a standalone pool (--endpoint-selector); pass --pool-name")
	}
	if len(res.objectives)+len(res.objectivesV1a2)+len(res.rewrites) > 0 {
		return nil, common.GKNN{}, errors.New("InferenceObjective and InferenceModelRewrite manifests have no effect with --endpoint-selector: " +
			"the router runs their reconcilers only for an InferencePool (--pool-name)")
	}
	pool := datalayer.NewEndpointPool(ns, "epp")
	pool.Selector = o.EndpointSelector
	pool.TargetPorts = append(pool.TargetPorts, o.EndpointTargetPorts...)
	gknn := common.GKNN{NamespacedName: types.NamespacedName{Namespace: ns, Name: "epp"}, GroupKind: schema.GroupKind{Kind: "Deployment", Group: "apps"}}
	return pool, gknn, nil
}

// reconcileResources runs the router's own objective and rewrite reconcilers over the manifests,
// as the Kubernetes controllers do at startup.
func (r *Router) reconcileResources(gknn common.GKNN, res *resources, objs []client.Object) error {
	objRec := &controller.InferenceObjectiveReconciler{
		Reader: r.kube, Datastore: r.emb.Datastore, PoolGKNN: gknn,
		PriorityBandControlPlane: r.emb.ControlPlane, PrimaryV1: true,
		// As the runner discovers from the API server: the installed CRD serves both versions.
		WatchV1Alpha2: res.served[schema.GroupVersion(v1alpha2.GroupVersion).WithKind("InferenceObjective")],
	}
	rwRec := &controller.InferenceModelRewriteReconciler{Reader: r.kube, Datastore: r.emb.Datastore, PoolGKNN: gknn}
	for _, o := range objs {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}}
		var err error
		switch o.(type) {
		case *corev1.Node, *giev1.InferencePool:
			// The objective and rewrite reconcilers do not reconcile them.
		case *v1alpha2.InferenceModelRewrite:
			_, err = rwRec.Reconcile(r.ctx, req)
		default:
			_, err = objRec.Reconcile(r.ctx, req)
		}
		if err != nil {
			return fmt.Errorf("reconciling %s/%s: %w", o.GetNamespace(), o.GetName(), err)
		}
	}
	return nil
}

// flowControl is how long the router can hold a request: the largest bounded queue-wait budget
// (the controller's default and no-endpoint budgets, every band's own) and the expiry sweep. An
// unbounded budget (an explicit zero) is left out: no router timer releases a request under it.
func flowControl(emb *runner.Embedded) contract.FlowControl {
	if !emb.FlowControlEnabled() || emb.Config == nil || emb.Config.FlowControlConfig == nil {
		return contract.FlowControl{}
	}
	fc := emb.Config.FlowControlConfig
	out := contract.FlowControl{Enabled: true, ExpirySweep: time.Second} // the controller's default cleanup interval
	var budgets []time.Duration
	if c := fc.Controller; c != nil {
		budgets = append(budgets, c.DefaultRequestTTL, c.NoEndpointRequestTTL)
		if c.ExpiryCleanupInterval > 0 {
			out.ExpirySweep = c.ExpiryCleanupInterval
		}
	}
	if reg := fc.Registry; reg != nil {
		bands := []*fcregistry.PriorityBandConfig{reg.DefaultPriorityBand, reg.DefaultNegativePriorityBand}
		for _, b := range reg.PriorityBands { // only the maximum is kept: order does not matter
			bands = append(bands, b)
		}
		for _, b := range bands {
			if b != nil && b.DefaultRequestTTL != nil {
				budgets = append(budgets, *b.DefaultRequestTTL)
			}
		}
	}
	for _, d := range budgets {
		if d > 0 {
			out.MaxQueueTTL = max(out.MaxQueueTTL, d)
		}
	}
	return out
}

// idleSkippable reports whether every background loop's effect over an idle period is what its
// last ticks produce. A data source polling at its own cadence counts base ticks, and the
// soft-reflective-ceiling policy counts dispatch cycles in a regime an idle pool can be in.
func idleSkippable(emb *runner.Embedded) bool {
	if emb.Config != nil && emb.Config.DataConfig != nil {
		for _, src := range emb.Config.DataConfig.Sources {
			if disp, ok := src.Plugin.(fwkdl.PollingDispatcher); ok && disp.Interval() > 0 {
				return false
			}
		}
	}
	if emb.FlowControlEnabled() && emb.Config != nil && emb.Config.FlowControlConfig != nil &&
		emb.Config.FlowControlConfig.UsageLimitPolicy != nil &&
		emb.Config.FlowControlConfig.UsageLimitPolicy.TypedName().Type == "soft-reflective-ceiling-policy" {
		return false
	}
	return true
}

// Requirements implements embed.Router.
func (r *Router) Requirements() contract.Requirements { return r.requirements }

// Pool implements embed.Router.
func (r *Router) Pool() contract.Pool {
	p := r.pool
	p.TargetPorts = slices.Clone(p.TargetPorts)
	return p
}

// FlowControl implements embed.Router.
func (r *Router) FlowControl() contract.FlowControl { return r.flowControl }

// IdleSkippable implements embed.Router.
func (r *Router) IdleSkippable() bool { return r.idleSkip }

// SetEndpoint implements embed.Router: the endpoint is a pod of the simulated cluster, made ready
// or not ready, and the router's own pod reconciler updates the pool.
func (r *Router) SetEndpoint(ep contract.Endpoint, ready bool) error {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod, ok := r.endpoints[ep.Name]
	if !ok {
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: ep.Name, Namespace: r.pool.Namespace, Labels: maps.Clone(ep.Labels)},
			Spec:       corev1.PodSpec{NodeName: ep.NodeName},
			Status: corev1.PodStatus{PodIP: ep.IP, HostIP: ep.IP, Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
		}
		if err := r.kube.Create(r.ctx, pod); err != nil {
			return fmt.Errorf("create pod %s: %w", ep.Name, err)
		}
		r.endpoints[ep.Name] = pod
		return r.reconcilePod(pod, false)
	}
	matched := r.emb.Datastore.PoolLabelsMatch(pod.Labels)
	metaChanged := !maps.Equal(pod.Labels, ep.Labels) || pod.Spec.NodeName != ep.NodeName
	statusChanged := pod.Status.PodIP != ep.IP || !podReady(pod, status)
	if !metaChanged && !statusChanged {
		return nil
	}
	if metaChanged {
		pod.Labels = maps.Clone(ep.Labels)
		pod.Spec.NodeName = ep.NodeName
		if err := r.kube.Update(r.ctx, pod); err != nil {
			return fmt.Errorf("update pod %s: %w", ep.Name, err)
		}
	}
	if statusChanged {
		pod.Status.PodIP, pod.Status.HostIP = ep.IP, ep.IP
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
		// Readiness is pod status, a subresource: a plain Update does not change it.
		if err := r.kube.Status().Update(r.ctx, pod); err != nil {
			return fmt.Errorf("update pod status %s: %w", ep.Name, err)
		}
	}
	return r.reconcilePod(pod, matched)
}

func podReady(pod *corev1.Pod, status corev1.ConditionStatus) bool {
	return len(pod.Status.Conditions) == 1 && pod.Status.Conditions[0].Type == corev1.PodReady && pod.Status.Conditions[0].Status == status
}

// reconcilePod runs the router's pod reconciler on pod, if the pool selects it or selected it.
func (r *Router) reconcilePod(pod *corev1.Pod, matched bool) error {
	if !matched && !r.emb.Datastore.PoolLabelsMatch(pod.Labels) {
		return nil
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}}
	if _, err := r.pods.Reconcile(r.ctx, req); err != nil {
		return fmt.Errorf("reconcile pod %s: %w", pod.Name, err)
	}
	return nil
}

// RemoveEndpoint implements embed.Router.
func (r *Router) RemoveEndpoint(name string) error {
	pod, ok := r.endpoints[name]
	if !ok {
		return nil
	}
	delete(r.endpoints, name)
	matched := r.emb.Datastore.PoolLabelsMatch(pod.Labels)
	if err := r.kube.Delete(r.ctx, pod); err != nil {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}
	return r.reconcilePod(pod, matched)
}

// Stop implements embed.Router: it cancels every request the router holds, in request-ID order,
// then the router's lifetime, which ends every loop the router started. A request still
// undecided is never decided.
func (r *Router) Stop() {
	if r.cancel == nil || r.stopped {
		return
	}
	r.stopped = true
	r.stopping()
	ids := make([]string, 0, len(r.flights))
	for id := range r.flights { // sorted below
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r.flights[id].cancel()
	}
	r.cancel()
}
