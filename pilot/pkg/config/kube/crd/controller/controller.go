// Copyright 2017 Istio Authors
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

package controller

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"istio.io/pkg/log"

	"istio.io/istio/pilot/pkg/config/kube/crd"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	controller2 "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config/schema"
	"istio.io/pkg/monitoring"
)

// controller is a collection of synchronized resource watchers.
// Caches are thread-safe
type controller struct {
	client *Client
	queue  kube.Queue
	kinds  map[string]cacheHandler
}

type cacheHandler struct {
	informer cache.SharedIndexInformer
	handler  *kube.ChainHandler
}

type ValidateFunc func(interface{}) error

var (
	typeTag  = monitoring.MustCreateLabel("type")
	eventTag = monitoring.MustCreateLabel("event")
	nameTag  = monitoring.MustCreateLabel("name")

	// experiment on getting some monitoring on config errors.
	k8sEvents = monitoring.NewSum(
		"pilot_k8s_cfg_events",
		"Events from k8s config.",
		monitoring.WithLabels(typeTag, eventTag),
	)

	k8sErrors = monitoring.NewGauge(
		"pilot_k8s_object_errors",
		"Errors converting k8s CRDs",
		monitoring.WithLabels(nameTag),
	)

	// InvalidCRDs contains a sync.Map keyed by the namespace/name of the entry, and has the error as value.
	// It can be used by tools like ctrlz to display the errors.
	InvalidCRDs atomic.Value
)

func init() {
	monitoring.MustRegister(k8sEvents, k8sErrors)
}

// NewController creates a new Kubernetes controller for CRDs
// Use "" for namespace to listen for all namespace changes
func NewController(client *Client, options controller2.Options) model.ConfigStoreCache {
	log.Infof("CRD controller watching namespaces %q", options.WatchedNamespace)

	// Queue requires a time duration for a retry delay after a handler error
	out := &controller{
		client: client,
		queue:  kube.NewQueue(1 * time.Second),
		kinds:  make(map[string]cacheHandler),
	}

	// add stores for CRD kinds
	for _, s := range client.ConfigDescriptor() {
		out.addInformer(s, options.WatchedNamespace, options.ResyncPeriod)
	}

	return out
}

func (c *controller) addInformer(schema schema.Instance, namespace string, resyncPeriod time.Duration) {
	c.kinds[schema.Type] = c.createInformer(crd.KnownTypes[schema.Type].Object.DeepCopyObject(), schema.Type, resyncPeriod,
		func(opts meta_v1.ListOptions) (result runtime.Object, err error) {
			result = crd.KnownTypes[schema.Type].Collection.DeepCopyObject()
			rc, ok := c.client.clientset[crd.APIVersion(&schema)]
			if !ok {
				return nil, fmt.Errorf("client not initialized %s", schema.Type)
			}
			req := rc.dynamic.Get().
				Resource(crd.ResourceName(schema.Plural)).
				VersionedParams(&opts, meta_v1.ParameterCodec)

			if !schema.ClusterScoped {
				req = req.Namespace(namespace)
			}
			err = req.Do().Into(result)
			return
		},
		func(opts meta_v1.ListOptions) (watch.Interface, error) {
			rc, ok := c.client.clientset[crd.APIVersion(&schema)]
			if !ok {
				return nil, fmt.Errorf("client not initialized %s", schema.Type)
			}
			opts.Watch = true
			req := rc.dynamic.Get().
				Resource(crd.ResourceName(schema.Plural)).
				VersionedParams(&opts, meta_v1.ParameterCodec)
			if !schema.ClusterScoped {
				req = req.Namespace(namespace)
			}
			return req.Watch()
		},
		func(obj interface{}) error {
			rc, ok := c.client.clientset[crd.APIVersion(&schema)]
			if !ok {
				return fmt.Errorf("client not initialized %s", schema.Type)
			}
			s, exists := rc.descriptor.GetByType(schema.Type)
			if !exists {
				return fmt.Errorf("unrecognized type %q", schema.Type)
			}

			item, ok := obj.(crd.IstioObject)
			if !ok {
				return fmt.Errorf("error convert %v to istio CRD", obj)
			}

			config, err := crd.ConvertObject(s, item, c.client.domainSuffix)
			if err != nil {
				return fmt.Errorf("error translating object for schema %#v : %v\n Object:\n%#v", s, err, obj)
			}

			if err := s.Validate(config.Name, config.Namespace, config.Spec); err != nil {
				return fmt.Errorf("failed to validate CRD %v, error: %v", config, err)
			}

			return nil
		})
}

// notify is the first handler in the handler chain.
// Returning an error causes repeated execution of the entire chain.
func (c *controller) notify(obj interface{}, event model.Event) error {
	if !c.HasSynced() {
		return errors.New("waiting till full synchronization")
	}
	_, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Infof("Error retrieving key: %v", err)
	}
	return nil
}

func (c *controller) createInformer(
	o runtime.Object,
	otype string,
	resyncPeriod time.Duration,
	lf cache.ListFunc,
	wf cache.WatchFunc,
	vf ValidateFunc) cacheHandler {
	handler := &kube.ChainHandler{}
	handler.Append(c.notify)

	// TODO: finer-grained index (perf)
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{ListFunc: lf, WatchFunc: wf}, o,
		resyncPeriod, cache.Indexers{})

	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			// TODO: filtering functions to skip over un-referenced resources (perf)
			AddFunc: func(obj interface{}) {
				if err := vf(obj); err != nil {
					log.Errorf("failed to add CRD. New value: %v, error: %v", obj, err)
					incrementEvent(otype, "addfailure")
					return
				}
				incrementEvent(otype, "add")
				c.queue.Push(kube.NewTask(handler.Apply, obj, model.EventAdd))
			},
			UpdateFunc: func(old, cur interface{}) {
				if err := vf(cur); err != nil {
					incrementEvent(otype, "updatefailure")
					log.Errorf("failed to update CRD. New value: %v, error: %v", cur, err)
					return
				}
				if !reflect.DeepEqual(old, cur) {
					incrementEvent(otype, "update")
					c.queue.Push(kube.NewTask(handler.Apply, cur, model.EventUpdate))
				} else {
					incrementEvent(otype, "updatesame")
				}
			},
			DeleteFunc: func(obj interface{}) {
				incrementEvent(otype, "delete")
				c.queue.Push(kube.NewTask(handler.Apply, obj, model.EventDelete))
			},
		})

	return cacheHandler{informer: informer, handler: handler}
}

func incrementEvent(kind, event string) {
	k8sEvents.With(typeTag.Value(kind), eventTag.Value(event)).Increment()
}

func (c *controller) RegisterEventHandler(typ string, f func(model.Config, model.Event)) {
	s, exists := c.ConfigDescriptor().GetByType(typ)
	if !exists {
		return
	}
	c.kinds[typ].handler.Append(func(object interface{}, ev model.Event) error {
		item, ok := object.(crd.IstioObject)
		if ok {
			config, err := crd.ConvertObject(s, item, c.client.domainSuffix)
			if err != nil {
				log.Warnf("error translating object for schema %#v : %v\n Object:\n%#v", s, err, object)
			} else {
				f(*config, ev)
			}
		}
		return nil
	})
}

func (c *controller) Version() string {
	return c.client.Version()
}

func (c *controller) GetResourceAtVersion(version string, key string) (resourceVersion string, err error) {
	return c.client.GetResourceAtVersion(version, key)
}

func (c *controller) HasSynced() bool {
	for kind, ctl := range c.kinds {
		if !ctl.informer.HasSynced() {
			log.Infof("controller %q is syncing...", kind)
			return false
		}
	}
	return true
}

func (c *controller) Run(stop <-chan struct{}) {
	go func() {
		cache.WaitForCacheSync(stop, c.HasSynced)
		c.queue.Run(stop)
	}()

	for _, ctl := range c.kinds {
		go ctl.informer.Run(stop)
	}

	<-stop
	log.Info("controller terminated")
}

func (c *controller) ConfigDescriptor() schema.Set {
	return c.client.ConfigDescriptor()
}

func (c *controller) Get(typ, name, namespace string) *model.Config {
	s, exists := c.client.ConfigDescriptor().GetByType(typ)
	if !exists {
		return nil
	}

	store := c.kinds[typ].informer.GetStore()
	data, exists, err := store.GetByKey(kube.KeyFunc(name, namespace))
	if !exists {
		return nil
	}
	if err != nil {
		log.Warna(err)
		return nil
	}

	obj, ok := data.(crd.IstioObject)
	if !ok {
		log.Warn("Cannot convert to config from store")
		return nil
	}

	config, err := crd.ConvertObject(s, obj, c.client.domainSuffix)
	if err != nil {
		return nil
	}

	return config
}

func (c *controller) Create(config model.Config) (string, error) {
	return c.client.Create(config)
}

func (c *controller) Update(config model.Config) (string, error) {
	return c.client.Update(config)
}

func (c *controller) Delete(typ, name, namespace string) error {
	return c.client.Delete(typ, name, namespace)
}

func (c *controller) List(typ, namespace string) ([]model.Config, error) {
	s, ok := c.client.ConfigDescriptor().GetByType(typ)
	if !ok {
		return nil, fmt.Errorf("missing type %q", typ)
	}

	var newErrors sync.Map
	var errs error
	out := make([]model.Config, 0)
	oldMap := InvalidCRDs.Load()
	if oldMap != nil {
		oldMap.(*sync.Map).Range(func(key, value interface{}) bool {
			k8sErrors.With(nameTag.Value(key.(string))).Record(1)
			return true
		})
	}
	for _, data := range c.kinds[typ].informer.GetStore().List() {
		item, ok := data.(crd.IstioObject)
		if !ok {
			continue
		}

		if namespace != "" && namespace != item.GetObjectMeta().Namespace {
			continue
		}

		config, err := crd.ConvertObject(s, item, c.client.domainSuffix)
		if err != nil {
			key := item.GetObjectMeta().Namespace + "/" + item.GetObjectMeta().Name
			log.Errorf("Failed to convert %s object, ignoring: %s %v %v", typ, key, err, item.GetSpec())
			// DO NOT RETURN ERROR: if a single object is bad, it'll be ignored (with a log message), but
			// the rest should still be processed.
			// TODO: find a way to reset and represent the error !!
			newErrors.Store(key, err)
			k8sErrors.With(nameTag.Value(key)).Record(1)
		} else {
			out = append(out, *config)
		}
	}
	InvalidCRDs.Store(&newErrors)
	return out, errs
}
