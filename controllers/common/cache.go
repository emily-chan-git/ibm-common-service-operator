//
// Copyright 2022 IBM Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package common

import (
	"context"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	filteredcache "github.com/IBM/controller-filtered-cache/filteredcache"
)

// NewCSCache implements a customized cache with a for CS
func NewCSCache(clusterGVKList []schema.GroupVersionKind, gvkLabelMap map[schema.GroupVersionKind]filteredcache.Selector, watchNamespaceList []string) cache.NewCacheFunc {
	return func(config *rest.Config, opts cache.Options) (cache.Cache, error) {

		// Get the frequency that informers are resynced
		var resync time.Duration
		if opts.Resync != nil {
			resync = *opts.Resync
		}

		// Generate informermap to contain the gvks and their informers
		informerMap, err := buildInformerMap(config, opts, resync, clusterGVKList)
		if err != nil {
			return nil, err
		}

		var NewCache cache.NewCacheFunc
		if watchNamespaceList[0] == "" {
			NewCache = filteredcache.NewFilteredCacheBuilder(gvkLabelMap)
		} else {
			NewCache = filteredcache.MultiNamespacedFilteredCacheBuilder(gvkLabelMap, watchNamespaceList)
		}

		// Create a default cache for the other resources
		fallback, err := NewCache(config, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to init fallback cache: %v", err)
		}

		// Return the customized cache
		return CSCache{config: config, informerMap: informerMap, fallback: fallback, Scheme: opts.Scheme}, nil
	}
}

//buildInformerMap generates informerMap of the specified resource
func buildInformerMap(config *rest.Config, opts cache.Options, resync time.Duration, clusterGVKList []schema.GroupVersionKind) (map[schema.GroupVersionKind]toolscache.SharedIndexInformer, error) {
	// Initialize informerMap
	informerMap := make(map[schema.GroupVersionKind]toolscache.SharedIndexInformer)

	for _, gvk := range clusterGVKList {

		// Create ListerWatcher by NewFilteredListWatchFromClient
		client, err := getClientForGVK(gvk, config, opts.Scheme)
		if err != nil {
			return nil, err
		}

		// Get the plural type of the kind as resource
		plural := kindToResource(gvk.Kind)
		listerWatcher := toolscache.NewFilteredListWatchFromClient(client, plural, opts.Namespace, func(options *metav1.ListOptions) {})

		// Build typed runtime object for informer
		objType := &unstructured.Unstructured{}
		objType.GetObjectKind().SetGroupVersionKind(gvk)
		typed, err := opts.Scheme.New(gvk)
		if err != nil {
			return nil, err
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objType.UnstructuredContent(), typed); err != nil {
			return nil, err
		}

		// Create new inforemer with the listerwatcher
		informer := toolscache.NewSharedIndexInformer(listerWatcher, typed, resync, toolscache.Indexers{toolscache.NamespaceIndex: toolscache.MetaNamespaceIndexFunc})
		informerMap[gvk] = informer
		// Build list type for the GVK
		gvkList := schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"}
		informerMap[gvkList] = informer
	}

	return informerMap, nil
}

// CSCache is the customized cache for CS
type CSCache struct {
	config      *rest.Config
	informerMap map[schema.GroupVersionKind]toolscache.SharedIndexInformer
	fallback    cache.Cache
	Scheme      *runtime.Scheme
}

// Get implements Reader
// If the resource is in the cache, Get function get fetch in from the informer
// Otherwise, resource will be get by the k8s client
func (c CSCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object) error {

	// Get the GVK of the client object
	gvk, err := apiutil.GVKForObject(obj, c.Scheme)
	if err != nil {
		return err
	}

	if informer, ok := c.informerMap[gvk]; ok {
		// Looking for object from the cache
		if err := c.getFromStore(informer, key, obj, gvk); err == nil {
			// If not found the object from cache, then fetch it from k8s apiserver
		} else if err := c.getFromClient(ctx, key, obj, gvk); err != nil {
			return err
		}
		return nil
	}

	// Passthrough
	return c.fallback.Get(ctx, key, obj)
}

// getFromStore gets the resource from the cache
func (c CSCache) getFromStore(informer toolscache.SharedIndexInformer, key client.ObjectKey, obj runtime.Object, gvk schema.GroupVersionKind) error {

	// Different key for cluster scope resource and namespaced resource
	var keyString string
	if key.Namespace == "" {
		keyString = key.Name
	} else {
		keyString = key.Namespace + "/" + key.Name
	}

	item, exists, err := informer.GetStore().GetByKey(keyString)
	if err != nil {
		klog.Info("Failed to get item from cache", "error", err)
		return err
	}
	if !exists {
		return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, key.String())
	}
	if _, isObj := item.(runtime.Object); !isObj {
		// This should never happen
		return fmt.Errorf("cache contained %T, which is not an Object", item)
	}

	// deep copy to avoid mutating cache
	item = item.(runtime.Object).DeepCopyObject()

	// Copy the value of the item in the cache to the returned value
	objVal := reflect.ValueOf(obj)
	itemVal := reflect.ValueOf(item)
	if !objVal.Type().AssignableTo(objVal.Type()) {
		return fmt.Errorf("cache had type %s, but %s was asked for", itemVal.Type(), objVal.Type())
	}
	reflect.Indirect(objVal).Set(reflect.Indirect(itemVal))
	obj.GetObjectKind().SetGroupVersionKind(gvk)

	return nil
}

// getFromClient gets the resource by the k8s client
func (c CSCache) getFromClient(ctx context.Context, key client.ObjectKey, obj runtime.Object, gvk schema.GroupVersionKind) error {

	// Get resource by the kubeClient
	resource := kindToResource(gvk.Kind)

	client, err := getClientForGVK(gvk, c.config, c.Scheme)
	if err != nil {
		return err
	}
	result, err := client.
		Get().
		NamespaceIfScoped(key.Namespace, key.Namespace != "").
		Name(key.Name).
		Resource(resource).
		VersionedParams(&metav1.GetOptions{}, metav1.ParameterCodec).
		Do(ctx).
		Get()

	if apierrors.IsNotFound(err) {
		return err
	} else if err != nil {
		klog.Info("Failed to retrieve resource list", "error", err)
		return err
	}

	// Copy the value of the item in the cache to the returned value
	objVal := reflect.ValueOf(obj)
	itemVal := reflect.ValueOf(result)
	if !objVal.Type().AssignableTo(objVal.Type()) {
		return fmt.Errorf("cache had type %s, but %s was asked for", itemVal.Type(), objVal.Type())
	}
	reflect.Indirect(objVal).Set(reflect.Indirect(itemVal))
	obj.GetObjectKind().SetGroupVersionKind(gvk)

	return nil
}

// List lists items out of the indexer and writes them to list
func (c CSCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	gvk, err := apiutil.GVKForObject(list, c.Scheme)
	if err != nil {
		return err
	}
	if informer, ok := c.informerMap[gvk]; ok {

		var objList []interface{}

		listOpts := client.ListOptions{}
		listOpts.ApplyOptions(opts)

		// Check the labelSelector
		var labelSel labels.Selector
		if listOpts.LabelSelector != nil {
			labelSel = listOpts.LabelSelector
		}

		if listOpts.FieldSelector != nil {
			// combining multiple indices, GetIndexers, etc
			field, val, requiresExact := requiresExactMatch(listOpts.FieldSelector)
			if !requiresExact {
				return fmt.Errorf("non-exact field matches are not supported by the cache")
			}
			// list all objects by the field selector.  If this is namespaced and we have one, ask for the
			// namespaced index key.  Otherwise, ask for the non-namespaced variant by using the fake "all namespaces"
			// namespace.
			objList, err = informer.GetIndexer().ByIndex(FieldIndexName(field), KeyToNamespacedKey(listOpts.Namespace, val))
		} else if listOpts.Namespace != "" {
			objList, err = informer.GetIndexer().ByIndex(toolscache.NamespaceIndex, listOpts.Namespace)
		} else {
			objList = informer.GetIndexer().List()
		}
		if err != nil {
			return err
		}

		// Check namespace and labelSelector
		runtimeObjList := make([]runtime.Object, 0, len(objList))
		for _, item := range objList {
			obj, isObj := item.(runtime.Object)
			if !isObj {
				return fmt.Errorf("cache contained %T, which is not an Object", obj)
			}
			meta, err := apimeta.Accessor(obj)
			if err != nil {
				return err
			}

			var namespace string

			if listOpts.Namespace != "" {
				namespace = listOpts.Namespace
			}

			if namespace != "" && namespace != meta.GetNamespace() {
				continue
			}

			if labelSel != nil {
				lbls := labels.Set(meta.GetLabels())
				if !labelSel.Matches(lbls) {
					continue
				}
			}

			outObj := obj.DeepCopyObject()
			outObj.GetObjectKind().SetGroupVersionKind(listToGVK(gvk))
			runtimeObjList = append(runtimeObjList, outObj)
		}
		return apimeta.SetList(list, runtimeObjList)
	}

	// Passthrough
	return c.fallback.List(ctx, list, opts...)
}

// GetInformer fetches or constructs an informer for the given object that corresponds to a single
// API kind and resource.
func (c CSCache) GetInformer(ctx context.Context, obj client.Object) (cache.Informer, error) {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme)
	if err != nil {
		return nil, err
	}

	if informer, ok := c.informerMap[gvk]; ok {
		return informer, nil
	}
	// Passthrough
	return c.fallback.GetInformer(ctx, obj)
}

// GetInformerForKind is similar to GetInformer, except that it takes a group-version-kind, instead
// of the underlying object.
func (c CSCache) GetInformerForKind(ctx context.Context, gvk schema.GroupVersionKind) (cache.Informer, error) {
	if informer, ok := c.informerMap[gvk]; ok {
		return informer, nil
	}
	// Passthrough
	return c.fallback.GetInformerForKind(ctx, gvk)
}

// Start runs all the informers known to this cache until the given channel is closed.
// It blocks.
func (c CSCache) Start(ctx context.Context) error {
	klog.Info("Start filtered cache")
	for _, informer := range c.informerMap {
		informer := informer
		go informer.Run(ctx.Done())
	}
	return c.fallback.Start(ctx)
}

// WaitForCacheSync waits for all the caches to sync.  Returns false if it could not sync a cache.
func (c CSCache) WaitForCacheSync(ctx context.Context) bool {
	// Wait for informer to sync
	waiting := true
	for waiting {
		select {
		case <-ctx.Done():
			waiting = false
		case <-time.After(time.Second):
			if len(c.informerMap) == 0 {
				waiting = false
			} else {
				currentWaiting := false
				for _, informer := range c.informerMap {
					currentWaiting = !informer.HasSynced() || currentWaiting
				}
				waiting = currentWaiting
			}
		}
	}
	// Wait for fallback cache to sync
	return c.fallback.WaitForCacheSync(ctx)
}

// IndexField adds an indexer to the underlying cache, using extraction function to get
// value(s) from the given field. The filtered cache doesn't support the index yet.
func (c CSCache) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme)
	if err != nil {
		return err
	}

	if informer, ok := c.informerMap[gvk]; ok {
		return indexByField(informer, field, extractValue)
	}

	return c.fallback.IndexField(ctx, obj, field, extractValue)
}

func indexByField(indexer cache.Informer, field string, extractor client.IndexerFunc) error {
	indexFunc := func(objRaw interface{}) ([]string, error) {
		// TODO(directxman12): check if this is the correct type?
		obj, isObj := objRaw.(client.Object)
		if !isObj {
			return nil, fmt.Errorf("object of type %T is not an Object", objRaw)
		}
		meta, err := apimeta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		ns := meta.GetNamespace()

		rawVals := extractor(obj)
		var vals []string
		if ns == "" {
			// if we're not doubling the keys for the namespaced case, just re-use what was returned to us
			vals = rawVals
		} else {
			// if we need to add non-namespaced versions too, double the length
			vals = make([]string, len(rawVals)*2)
		}
		for i, rawVal := range rawVals {
			// save a namespaced variant, so that we can ask
			// "what are all the object matching a given index *in a given namespace*"
			vals[i] = KeyToNamespacedKey(ns, rawVal)
			if ns != "" {
				// if we have a namespace, also inject a special index key for listing
				// regardless of the object namespace
				vals[i+len(rawVals)] = KeyToNamespacedKey("", rawVal)
			}
		}

		return vals, nil
	}

	return indexer.AddIndexers(toolscache.Indexers{FieldIndexName(field): indexFunc})
}

// kindToResource converts kind to resource
func kindToResource(kind string) string {
	kindToResourceMap := map[string]string{
		"MutatingWebhookConfiguration":   "mutatingwebhookconfigurations",
		"ValidatingWebhookConfiguration": "validatingwebhookconfigurations",
	}
	return kindToResourceMap[kind]
}

// listToGVK converts GVK list to GVK
func listToGVK(list schema.GroupVersionKind) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: list.Group, Version: list.Version, Kind: list.Kind[:len(list.Kind)-2]}
}

// requiresExactMatch checks if the given field selector is of the form `k=v` or `k==v`.
func requiresExactMatch(sel fields.Selector) (field, val string, required bool) {
	reqs := sel.Requirements()
	if len(reqs) != 1 {
		return "", "", false
	}
	req := reqs[0]
	if req.Operator != selection.Equals && req.Operator != selection.DoubleEquals {
		return "", "", false
	}
	return req.Field, req.Value, true
}

// FieldIndexName constructs the name of the index over the given field,
// for use with an indexer.
func FieldIndexName(field string) string {
	return "field:" + field
}

// noNamespaceNamespace is used as the "namespace" when we want to list across all namespaces
const allNamespacesNamespace = "__all_namespaces"

// KeyToNamespacedKey prefixes the given index key with a namespace
// for use in field selector indexes.
func KeyToNamespacedKey(ns string, baseKey string) string {
	if ns != "" {
		return ns + "/" + baseKey
	}
	return allNamespacesNamespace + "/" + baseKey
}

func getClientForGVK(gvk schema.GroupVersionKind, config *rest.Config, scheme *runtime.Scheme) (toolscache.Getter, error) {
	gv := gvk.GroupVersion()
	cfg := rest.CopyConfig(config)
	cfg.GroupVersion = &gv
	cfg.APIPath = "/apis"
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	if cfg.NegotiatedSerializer == nil {
		cfg.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: serializer.NewCodecFactory(scheme)}
	}
	return rest.RESTClientFor(cfg)
}
