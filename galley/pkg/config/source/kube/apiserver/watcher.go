// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiserver

import (
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"istio.io/istio/galley/pkg/config/event"
	"istio.io/istio/galley/pkg/config/resource"
	"istio.io/istio/galley/pkg/config/schema"
	"istio.io/istio/galley/pkg/config/scope"
	"istio.io/istio/galley/pkg/config/source/kube/apiserver/stats"
	"istio.io/istio/galley/pkg/config/source/kube/apiserver/tombstone"
	"istio.io/istio/galley/pkg/config/source/kube/rt"
)

type watcher struct {
	mu sync.Mutex

	adapter  *rt.Adapter
	resource schema.KubeResource

	handler event.Handler

	done chan struct{}
}

func newWatcher(r schema.KubeResource, a *rt.Adapter) *watcher {
	return &watcher{
		resource: r,
		adapter:  a,
	}
}

func (w *watcher) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		panic("watcher.start: already started")
	}

	scope.Source.Debugf("Starting watcher for %q (%q)", w.resource.Collection.Name, w.resource.CanonicalResourceName())

	informer, err := w.adapter.NewInformer()
	if err != nil {
		scope.Source.Errorf("unable to start watcher for %q: %v", w.resource.CanonicalResourceName(), err)
		w.handler.Handle(event.FullSyncFor(w.resource.Collection.Name))
		return
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { w.handleEvent(event.Added, obj) },
		UpdateFunc: func(old, new interface{}) {
			if w.adapter.IsEqual(old, new) {
				// Periodic resync will send update events for all known resources.
				// Two different versions of the same resource will always have different RVs.
				return
			}

			w.handleEvent(event.Updated, new)
		},
		DeleteFunc: func(obj interface{}) { w.handleEvent(event.Deleted, obj) },
	})

	done := make(chan struct{})
	w.done = done

	// Send the FullSync event after the cache syncs.
	go func() {
		_ = cache.WaitForCacheSync(done, informer.HasSynced)
		if w.handler != nil {
			w.handler.Handle(event.FullSyncFor(w.resource.Collection.Name))
		}
	}()

	// Start CRD shared informer and wait for it to exit.
	go informer.Run(done)
}

func (w *watcher) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		close(w.done)
		w.done = nil
	}
}

func (w *watcher) dispatch(h event.Handler) {
	w.handler = event.CombineHandlers(w.handler, h)
}

func (w *watcher) handleEvent(c event.Kind, obj interface{}) {
	object, ok := obj.(metav1.Object)
	if !ok {
		if obj = tombstone.RecoverResource(obj); object != nil {
			// Tombstone recovery failed.
			scope.Source.Warnf("Unable to extract object for event: %v", obj)
			return
		}
	}

	object = w.adapter.ExtractObject(obj)
	res, err := w.adapter.ExtractResource(obj)
	if err != nil {
		scope.Source.Warnf("unable to extract resource: %v: %e", obj, err)
		return
	}

	r := &resource.Entry{
		Metadata: resource.Metadata{
			Name:        resource.NewName(object.GetNamespace(), object.GetName()),
			Version:     resource.Version(object.GetResourceVersion()),
			Annotations: object.GetAnnotations(),
			Labels:      object.GetLabels(),
			CreateTime:  object.GetCreationTimestamp().Time,
		},
		Item: res,
	}

	e := event.Event{
		Kind:   c,
		Source: w.resource.Collection.Name,
		Entry:  r,
	}

	if w.handler != nil {
		scope.Source.Debugf("Sending event: [%v] from: %s", e, w.resource.Collection.Name)
		w.handler.Handle(e)
	}
	stats.RecordEventSuccess()
}
