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

package datalayer

import (
	"context"
	"errors"
	"sort"

	"k8s.io/apimachinery/pkg/runtime/schema"

	fwkdl "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/datalayer"
)

// NotifyHosted delivers a Kubernetes object notification that a host produced to the
// notification sources of the object's kind and to their extractors, by the path a watch takes
// (notificationReconciler.dispatch).
//
// A router that runs in a process of its own binds each notification source to a
// controller-runtime watch in Start. An embedded router never calls Start, so nothing delivers
// those notifications; the embedding calls NotifyHosted for the objects it creates, updates and
// deletes. Sources are served in name order, so the order of delivery does not depend on map
// order. It returns the joined errors of the sources and extractors that failed.
func (r *Runtime) NotifyHosted(ctx context.Context, gvk schema.GroupVersionKind, event fwkdl.NotificationEvent) error {
	var names []string
	r.notification.Range(func(name string, src fwkdl.NotificationSource) bool {
		if src.GVK() == gvk {
			names = append(names, name)
		}
		return true
	})
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		src, _ := r.notification.Get(name)
		var extractors []fwkdl.NotificationExtractor
		if raw, ok := r.extractors.Get(name); ok {
			for _, e := range raw {
				extractors = append(extractors, e.(fwkdl.NotificationExtractor))
			}
		}
		rec := &notificationReconciler{src: src, extractors: extractors, gvk: gvk, log: r.logger}
		if err := rec.dispatch(ctx, r.logger, &event); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
