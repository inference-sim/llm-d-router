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
	"fmt"
	"sort"

	contract "github.com/inference-sim/blis-substrate/contracts/embed"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
)

// podGVK is the kind of the objects SetEndpoint creates.
var podGVK = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}

// notifyPod delivers the pod's creation, change or deletion to the router's Pod notification
// sources, as the watch a deployed router binds in datalayer.Runtime.Start would. An embedded
// router never starts that watch, so without this a plugin that learns about pods from
// notifications (the disaggregatedset-rollout-screener) would see none.
func (r *Router) notifyPod(typ fwkdl.EventType, pod *corev1.Pod) error {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		return fmt.Errorf("convert pod %s for notification: %w", pod.Name, err)
	}
	obj := &unstructured.Unstructured{Object: content}
	obj.SetGroupVersionKind(podGVK)
	if err := r.emb.DataLayer.NotifyHosted(r.ctx, podGVK, fwkdl.NotificationEvent{Type: typ, Object: obj}); err != nil {
		return fmt.Errorf("notify pod %s: %w", pod.Name, err)
	}
	return nil
}

// podGuard checks the labels of a pod for one plugin.
type podGuard struct {
	plugin fwkplugin.TypedName
	check  func(labels map[string]string) error
}

// podGuards finds the plugins that can say a pod would be invisible to them, in name order.
func podGuards(handle fwkplugin.Handle) []podGuard {
	var out []podGuard
	for name, p := range handle.GetAllPluginsWithNames() { // sorted below
		if g, ok := p.(interface{ CheckHostedPod(map[string]string) error }); ok {
			out = append(out, podGuard{plugin: fwkplugin.TypedName{Type: p.TypedName().Type, Name: name}, check: g.CheckHostedPod})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].plugin.Name < out[j].plugin.Name })
	return out
}

// checkPod refuses a pod the router would route to that a plugin would silently drop from every
// request: an error naming the pod's labels and the plugin, before the pod exists.
func (r *Router) checkPod(name string, labels map[string]string) error {
	if len(r.podGuard) == 0 || !r.emb.Datastore.PoolLabelsMatch(labels) {
		return nil
	}
	for _, g := range r.podGuard {
		if err := g.check(labels); err != nil {
			return fmt.Errorf(refusedPrefix+"%w", &contract.RefusalError{Plugin: g.plugin.Name, Type: g.plugin.Type, Reason: "pod " + name + ": " + err.Error()})
		}
	}
	return nil
}
