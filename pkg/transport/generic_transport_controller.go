/*
Copyright 2023 The KubeStellar Authors.

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

package transport

import (
	"context"
	"fmt"
	"go/token"
	"sync"
	"time"

	"github.com/go-logr/logr"
	clusterinformers "open-cluster-management.io/api/client/cluster/informers/externalversions/cluster/v1"
	clusterlisters "open-cluster-management.io/api/client/cluster/listers/cluster/v1"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/kubestellar/kubestellar/api/control/v1alpha1"
	a "github.com/kubestellar/kubestellar/pkg/abstract"
	"github.com/kubestellar/kubestellar/pkg/customize"
	controlclient "github.com/kubestellar/kubestellar/pkg/generated/clientset/versioned/typed/control/v1alpha1"
	controlv1alpha1informers "github.com/kubestellar/kubestellar/pkg/generated/informers/externalversions/control/v1alpha1"
	controlv1alpha1listers "github.com/kubestellar/kubestellar/pkg/generated/listers/control/v1alpha1"
	"github.com/kubestellar/kubestellar/pkg/transport/filtering"
	"github.com/kubestellar/kubestellar/pkg/util"
)

const (
	ControllerName                  = "transport-controller"
	transportFinalizer              = "transport.kubestellar.io/object-cleanup"
	originOwnerReferenceLabel       = "transport.kubestellar.io/originOwnerReferenceBindingKey"
	originWdsLabel                  = "transport.kubestellar.io/originWdsName"
	originOwnerGenerationAnnotation = "transport.kubestellar.io/originOwnerReferenceBindingGeneration"
)

// objectsFilter map from gvk to a filter function to clean specific fields from objects before adding them to a wrapped object.
var objectsFilter = filtering.NewObjectFilteringMap()

// NewTransportController returns a new transport controller.
// This func is like NewTransportControllerForWrappedObjectGVR but first uses
// the given transport and transportClientset to discover the GVR of wrapped objects.
// The given transportDynamicClient is used to access the ITS.
func NewTransportController(ctx context.Context,
	inventoryPreInformer clusterinformers.ManagedClusterInformer,
	bindingClient controlclient.BindingInterface, bindingInformer controlv1alpha1informers.BindingInformer, transport Transport,
	wdsDynamicClient dynamic.Interface, itsNSClient corev1client.NamespaceInterface, propCfgMapPreInformer corev1informers.ConfigMapInformer,
	transportClientset kubernetes.Interface,
	transportDynamicClient dynamic.Interface, wdsName string) (*genericTransportController, error) {
	emptyWrappedObject := transport.WrapObjects(make([]*unstructured.Unstructured, 0)) // empty wrapped object to get GVR from it.
	wrappedObjectGVR, err := getGvrFromWrappedObject(transportClientset, emptyWrappedObject)
	if err != nil {
		return nil, fmt.Errorf("failed to get wrapped object GVR - %w", err)
	}
	return NewTransportControllerForWrappedObjectGVR(ctx, inventoryPreInformer, bindingClient, bindingInformer, transport, wdsDynamicClient, itsNSClient, propCfgMapPreInformer, transportDynamicClient, wdsName, wrappedObjectGVR), nil
}

// NewTransportControllerForWrappedObjectGVR returns a new transport controller.
// The given transportDynamicClient is used to access the ITS.
func NewTransportControllerForWrappedObjectGVR(ctx context.Context,
	inventoryPreInformer clusterinformers.ManagedClusterInformer,
	bindingClient controlclient.BindingInterface, bindingInformer controlv1alpha1informers.BindingInformer, transport Transport,
	wdsDynamicClient dynamic.Interface,
	itsNSClient corev1client.NamespaceInterface,
	propCfgMapPreInformer corev1informers.ConfigMapInformer,
	transportDynamicClient dynamic.Interface, wdsName string, wrappedObjectGVR schema.GroupVersionResource) *genericTransportController {
	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(transportDynamicClient, 0)
	wrappedObjectGenericInformer := dynamicInformerFactory.ForResource(wrappedObjectGVR)

	transportController := &genericTransportController{
		logger:                       klog.FromContext(ctx),
		inventoryInformerSynced:      inventoryPreInformer.Informer().HasSynced,
		inventoryLister:              inventoryPreInformer.Lister(),
		bindingClient:                bindingClient,
		bindingLister:                bindingInformer.Lister(),
		bindingInformerSynced:        bindingInformer.Informer().HasSynced,
		itsNSClient:                  itsNSClient,
		propCfgMapLister:             propCfgMapPreInformer.Lister().ConfigMaps(v1alpha1.PropertyConfigMapNamespace),
		propCfgMapInformerSynced:     propCfgMapPreInformer.Informer().HasSynced,
		wrappedObjectInformerSynced:  wrappedObjectGenericInformer.Informer().HasSynced,
		workqueue:                    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName),
		transport:                    transport,
		transportClient:              transportDynamicClient,
		wrappedObjectGVR:             wrappedObjectGVR,
		wdsDynamicClient:             wdsDynamicClient,
		wdsName:                      wdsName,
		bindingSensitiveDestinations: make(map[string]sets.Set[v1alpha1.Destination]),
		destinationProperties:        make(map[v1alpha1.Destination]clusterProperties),
	}

	transportController.logger.Info("Setting up event handlers")
	// Set up an event handler for when Binding resources change
	bindingInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			transportController.handleBinding(obj, "add")
		},
		UpdateFunc: func(_, new interface{}) { transportController.handleBinding(new, "update") },
		DeleteFunc: func(obj any) {
			if dfsu, is := obj.(*cache.DeletedFinalStateUnknown); is {
				obj = dfsu.Obj
			}
			transportController.handleBinding(obj, "delete")
		},
	})
	// Set up event handlers for when WrappedObject resources change. The handlers will lookup the origin Binding
	// of the given WrappedObject and enqueue that Binding object for processing.
	// This way, we don't need to implement custom logic for handling WrappedObject resources. More info on this pattern:
	// https://github.com/kubernetes/community/blob/8cafef897a22026d42f5e5bb3f104febe7e29830/contributors/devel/controllers.md
	wrappedObjectGenericInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			transportController.handleWrappedObject(obj, "add")
		},
		UpdateFunc: func(_, new interface{}) {
			transportController.handleWrappedObject(new, "update")
		},
		DeleteFunc: func(obj any) {
			if dfsu, is := obj.(*cache.DeletedFinalStateUnknown); is {
				obj = dfsu.Obj
			}
			transportController.handleWrappedObject(obj, "delete")
		},
	})
	inventoryPreInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			transportController.handlePropertiesEvent(obj, "add")
		},
		UpdateFunc: func(old, new interface{}) {
			transportController.handlePropertiesEvent(new, "update")
		},
		DeleteFunc: func(obj any) {
			if dfsu, is := obj.(*cache.DeletedFinalStateUnknown); is {
				obj = dfsu.Obj
			}
			transportController.handlePropertiesEvent(obj, "delete")
		},
	})
	propCfgMapPreInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			transportController.handlePropertiesEvent(obj, "add")
		},
		UpdateFunc: func(old, new interface{}) {
			transportController.handlePropertiesEvent(new, "update")
		},
		DeleteFunc: func(obj any) {
			if dfsu, is := obj.(*cache.DeletedFinalStateUnknown); is {
				obj = dfsu.Obj
			}
			transportController.handlePropertiesEvent(obj, "delete")
		},
	})
	dynamicInformerFactory.Start(ctx.Done())

	return transportController
}

func convertObjectToUnstructured(object runtime.Object) (*unstructured.Unstructured, error) {
	unstructuredObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, fmt.Errorf("failed to convert given object to unstructured - %w", err)
	}
	return &unstructured.Unstructured{Object: unstructuredObject}, nil
}

func getGvrFromWrappedObject(clientset kubernetes.Interface, wrappedObject runtime.Object) (schema.GroupVersionResource, error) {
	unstructuredWrappedObject, err := convertObjectToUnstructured(wrappedObject)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("failed to convert wrapped object to unstructured - %w", err)
	}

	gvk := unstructuredWrappedObject.GroupVersionKind()
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cacheddiscovery.NewMemCacheClient(clientset.Discovery()))

	restMapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("failed to discover GroupVersionResource from given GroupVersionKind - %w", err)
	}

	return restMapping.Resource, nil
}

// clusterProperties holds the (name, value) pairs that are the properties
// of a given WEC, for input to customization.
type clusterProperties = map[string]string

type genericTransportController struct {
	logger logr.Logger

	inventoryInformerSynced     cache.InformerSynced
	inventoryLister             clusterlisters.ManagedClusterLister
	bindingClient               controlclient.BindingInterface
	bindingLister               controlv1alpha1listers.BindingLister
	bindingInformerSynced       cache.InformerSynced
	itsNSClient                 corev1client.NamespaceInterface
	propCfgMapLister            corev1listers.ConfigMapNamespaceLister
	propCfgMapInformerSynced    cache.InformerSynced
	wrappedObjectInformerSynced cache.InformerSynced

	// workqueue is a rate limited work queue of references to objects to work on.
	// This is used to queue work to be processed instead of performing it as soon as a change happens.
	// This means we can ensure we only process a fixed amount of resources at a time, and makes it
	// easy to ensure we are never processing the same item simultaneously in two different workers.
	// An item can be either of two types: a string holding the name of a Binding, or a
	// recollectProperties holding the name of a inventory object.
	workqueue workqueue.RateLimitingInterface

	transport        Transport         //transport is a specific implementation for the transport interface.
	transportClient  dynamic.Interface // dynamic client to transport wrapped object. since object kind is unknown during complilation, we use dynamic
	wrappedObjectGVR schema.GroupVersionResource

	wdsDynamicClient dynamic.Interface
	wdsName          string

	propsMutex sync.Mutex

	// bindingSensitiveDestinations maps Binding name to the set of destinations whose properties the Binding is senstive to.
	// Access to both the map and the Sets it holds is controlled by the RWMutex.
	// The sets are mutable with the RWMutex held.
	bindingSensitiveDestinations map[string]sets.Set[v1alpha1.Destination]

	// destinationProperties maps a destination to the properties to use for it in template expansion.
	// Access only while holding RWMutex and keep consistent with bindingSensitiveDestinations.
	// An entry is removed from this map when this controller is notified of
	// deletion of the destination's property ConfigMap.
	// Every `clusterProperties` that appears here is immutable from the time that it arrived.
	destinationProperties map[v1alpha1.Destination]clusterProperties
}

// enqueueBinding takes an Binding resource and
// converts it into a namespace/name string which is put onto the workqueue.
// This func *shouldn't* handle any resource other than Binding.
func (c *genericTransportController) handleBinding(obj interface{}, event string) {
	binding := obj.(*v1alpha1.Binding)
	c.logger.V(4).Info("Enqueuing reference to Binding due to informer event about that Binding", "name", binding.Name, "resourceVersion", binding.ResourceVersion, "event", event)
	c.workqueue.Add(binding.Name)
}

// handleWrappedObject takes transport-specific wrapped object resource,
// extracts the origin Binding of the given wrapped object and
// enqueue that Binding object for processing. This way, we
// don't need to implement custom logic for handling WrappedObject resources.
// More info on this pattern here:
// https://github.com/kubernetes/community/blob/8cafef897a22026d42f5e5bb3f104febe7e29830/contributors/devel/controllers.md
func (c *genericTransportController) handleWrappedObject(obj interface{}, event string) {
	wrappedObject := obj.(metav1.Object)
	ownerBindingKey, found := wrappedObject.GetLabels()[originOwnerReferenceLabel] // safe if GetLabels() returns nil
	if !found {
		c.logger.Info("failed to extract binding key from wrapped object", "wrappedObjectRef", cache.MetaObjectToName(wrappedObject), "resourceVersion", wrappedObject.GetResourceVersion(), "event", event)
		return
	}
	c.logger.V(4).Info("Enqueuing reference to Binding due to informer event about wrapped object", "bindingName", ownerBindingKey, "wrappedObjectRef", cache.MetaObjectToName(wrappedObject), "resourceVersion", wrappedObject.GetResourceVersion(), "event", event)
	// enqueue Binding key to trigger reconciliation.
	// if wrapped object was created not as a result of Binding,
	// the required annotation won't be found and nothing will happen.
	c.workqueue.Add(ownerBindingKey)
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until context
// is cancelled, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *genericTransportController) Run(ctx context.Context, workersCount int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	c.logger.Info("starting transport controller")
	go c.ensurePropertyNamespace(ctx)

	// Wait for the caches to be synced before starting workers
	c.logger.Info("waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.inventoryInformerSynced, c.bindingInformerSynced, c.wrappedObjectInformerSynced, c.propCfgMapInformerSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	c.logger.Info("starting workers", "count", workersCount)
	// Launch workers to process Binding
	for i := 1; i <= workersCount; i++ {
		workerId := i // in go, there is one `i` variable that gets different values in different iterations of the loop
		go wait.UntilWithContext(ctx, func(ctx context.Context) { c.runWorker(ctx, workerId) }, time.Second)
	}

	c.logger.Info("started workers")
	<-ctx.Done()
	c.logger.Info("shutting down workers")

	return nil
}

func (c *genericTransportController) ensurePropertyNamespace(ctx context.Context) {
	logger := klog.FromContext(ctx)
	for {
		_, err := c.itsNSClient.Get(ctx, v1alpha1.PropertyConfigMapNamespace, metav1.GetOptions{})
		if err == nil {
			logger.Info("Found property namespace already exists")
			return
		}
		if !errors.IsNotFound(err) {
			logger.Info("Failed to Get the property namespace", "err", err)
		} else {
			ns := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PropertyConfigMapNamespace},
			}
			_, err = c.itsNSClient.Create(ctx, &ns, metav1.CreateOptions{FieldManager: ControllerName})
			if err == nil {
				logger.Info("Created property namespace")
				return
			} else {
				logger.Info("Failed to create property namespace", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			logger.Info("Giving up on creating property namespace")
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *genericTransportController) runWorker(ctx context.Context, workerId int) {
	logger := klog.FromContext(ctx).WithValues("workerID", workerId)
	ctx = klog.NewContext(ctx, logger)
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *genericTransportController) processNextWorkItem(ctx context.Context) bool {
	logger := klog.FromContext(ctx)
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	var err error
	var retry bool
	defer func() {
		if err == nil {
			// If no error occurs we Forget this item so it does not
			// get queued again until another change happens.
			c.workqueue.Forget(obj)
			logger.Info("Processed workqueue item successfully.", "item", obj, "itemType", fmt.Sprintf("%T", obj))
		} else if retry {
			c.workqueue.AddRateLimited(obj)
			logger.V(5).Info("Encountered transient error while processing workqueue item; do not be alarmed, this will be retried later", "item", obj, "itemType", fmt.Sprintf("%T", obj))
		} else {
			c.workqueue.Forget(obj)
			logger.Error(err, "Failed to process workqueue item", "item", obj, "itemType", fmt.Sprintf("%T", obj))
		}
	}()
	err, retry = c.process(ctx, obj)
	return true
}

// recollectProperties is a queue entry requesting re-collecting
// a WEC's properties and, if they have changed, enqueuing references to the Bindings
// to which that matters. The string is the name of the inventory object.
type recollectProperties string

// process works on one object reference from the work queue.
// If the returned error is not nil then the returned boolean indicates
// whether to retry.
func (c *genericTransportController) process(ctx context.Context, obj interface{}) (error, bool) {
	// We call Done here so the workqueue knows we have finished processing this item.
	// We also must remember to call Forget if we do not want this work item being re-queued.
	// For example, we do not call Forget if a transient error occurs, instead the item is
	// put back on the workqueue and attempted again after a back-off period.
	defer c.workqueue.Done(obj)
	switch typed := obj.(type) {
	case string:
		_, objectName, err := cache.SplitMetaNamespaceKey(typed)
		if err != nil {
			// As the item in the workqueue is actually invalid, we call Forget here else we'd go
			// into a loop of attempting to process a work item that is invalid.
			return fmt.Errorf("invalid object key '%s' - %w", typed, err), false
		}

		// Run the syncHandler, passing it the Binding object name to be synced.
		if err := c.syncBinding(ctx, objectName); err != nil {
			return err, true
		}
		return nil, false

	case recollectProperties:
		c.syncProperties(ctx, string(typed))
		return nil, false

	default:
		return fmt.Errorf("expected workqueue item to be a string or recollectProperties but instead got %#v (type %T)", obj, obj), false
	}
}

// syncProperties checks whether the properties of a WEC have changed and, if so,
// enqueues references to all the Bindings to which that matters.
func (c *genericTransportController) syncProperties(ctx context.Context, invName string) {
	logger := klog.FromContext(ctx)
	newProps := c.collectPropertiesForDestination(logger, invName)
	c.propsMutex.Lock()
	defer c.propsMutex.Unlock()
	dest := v1alpha1.Destination{ClusterId: invName}
	oldProps, have := c.destinationProperties[dest]
	if !have { // not cached, nobody cares
		return
	}
	if a.PrimitiveMapEqual(oldProps, newProps) {
		return
	}
	c.logger.V(4).Info("syncProperties", "dest", dest, "props", newProps)
	c.destinationProperties[dest] = newProps
	for bindingName, dests := range c.bindingSensitiveDestinations {
		if dests.Has(dest) {
			c.logger.V(4).Info("Enqueuing reference to Binding that depends on changed destination properties", "binding", bindingName, "destination", dest)
			c.workqueue.Add(bindingName)
		}
	}
}

// syncBinding compares the actual state with the desired, and attempts to converge actual state to the desired state.
// returning an error from this function will result in a requeue of the given object key.
// therefore, if object shouldn't be requeued, don't return error.
func (c *genericTransportController) syncBinding(ctx context.Context, objectName string) error {
	// Get the Binding object with this name from WDS
	binding, err := c.bindingLister.Get(objectName)

	if errors.IsNotFound(err) { // the object was deleted and it had no finalizer on it. this means transport controller
		// finished cleanup of wrapped objects from mailbox namespaces. no need to do anything in this state.
		return nil
	}
	if err != nil { // in case of a different error, log it and retry.
		return fmt.Errorf("failed to get Binding object '%s' - %w", objectName, err)
	}

	if isObjectBeingDeleted(binding) {
		c.setBindingSensitivities(binding.Name, nil)
		return c.deleteWrappedObjectsAndFinalizer(ctx, binding)
	}
	// otherwise, object was not deleted and no error occurered while reading the object.
	return c.updateWrappedObjectsAndFinalizer(ctx, binding)
}

// isObjectBeingDeleted is a helper function to check if object is being deleted.
func isObjectBeingDeleted(object metav1.Object) bool {
	return !object.GetDeletionTimestamp().IsZero()
}

func (c *genericTransportController) deleteWrappedObjectsAndFinalizer(ctx context.Context, binding *v1alpha1.Binding) error {
	for _, destination := range binding.Spec.Destinations {
		if err := c.deleteWrappedObject(ctx, destination.ClusterId, fmt.Sprintf("%s-%s", binding.GetName(), c.wdsName)); err != nil {
			// wrapped object name is in the format (Binding.GetName()-WdsName). see updateWrappedObject func for explanation.
			return fmt.Errorf("failed to delete wrapped object from all destinations' - %w", err)
		}
	}

	if err := c.removeFinalizerFromBinding(ctx, binding); err != nil {
		return fmt.Errorf("failed to remove finalizer from Binding object '%s' - %w", binding.GetName(), err)
	}

	return nil
}

func (c *genericTransportController) deleteWrappedObject(ctx context.Context, namespace string, objectName string) error {
	err := c.transportClient.Resource(c.wrappedObjectGVR).Namespace(namespace).Delete(ctx, objectName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) { // if object is already not there, we do not report an error cause desired state was achieved.
		return fmt.Errorf("failed to delete wrapped object '%s' from destination WEC mailbox namespace '%s' - %w", objectName, namespace, err)
	}
	return nil
}

func (c *genericTransportController) removeFinalizerFromBinding(ctx context.Context, binding *v1alpha1.Binding) error {
	return c.updateBinding(ctx, binding, func(binding *v1alpha1.Binding) (*v1alpha1.Binding, bool) {
		return removeFinalizer(binding, transportFinalizer)
	})
}

func (c *genericTransportController) addFinalizerToBinding(ctx context.Context, binding *v1alpha1.Binding) error {
	return c.updateBinding(ctx, binding, func(binding *v1alpha1.Binding) (*v1alpha1.Binding, bool) {
		return addFinalizer(binding, transportFinalizer)
	})
}

func (c *genericTransportController) updateWrappedObjectsAndFinalizer(ctx context.Context, binding *v1alpha1.Binding) error {
	if err := c.addFinalizerToBinding(ctx, binding); err != nil {
		return fmt.Errorf("failed to add finalizer to Binding object '%s' - %w", binding.GetName(), err)
	}
	// get current state
	currentWrappedObjectList, err := c.transportClient.Resource(c.wrappedObjectGVR).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", originOwnerReferenceLabel, binding.GetName(), originWdsLabel, c.wdsName),
	})
	if err != nil {
		return fmt.Errorf("failed to get current wrapped objects that are owned by Binding '%s' - %w", binding.GetName(), err)
	}
	// calculate desired state
	destToDesiredWrappedObject, bindingErrors, err := c.computeDestToWrappedObjects(ctx, binding)
	if err != nil {
		return fmt.Errorf("failed to build wrapped object(s) from Binding '%s' - %w", binding.GetName(), err)
	}
	if binding.Status.ObservedGeneration != binding.Generation || !a.SliceEqual(binding.Status.Errors, bindingErrors) {
		bindingCopy := binding.DeepCopy()
		bindingCopy.Status = v1alpha1.BindingStatus{
			ObservedGeneration: binding.Generation,
			Errors:             bindingErrors,
		}
		binding2, err := c.bindingClient.UpdateStatus(ctx, bindingCopy, metav1.UpdateOptions{FieldManager: ControllerName})
		if err != nil {
			return fmt.Errorf("failed to update status of Binding '%s' - %w", binding.Name, err)
		} else {
			klog.FromContext(ctx).V(3).Info("Updated BindingStatus", "bindingName", binding.Name, "resourceVersion", binding2.ResourceVersion)
		}
	}
	// converge actual state to the desired state
	if err := c.propagateWrappedObjectToClusters(ctx, destToDesiredWrappedObject, currentWrappedObjectList, binding.Spec.Destinations, len(bindingErrors) != 0); err != nil {
		return fmt.Errorf("failed to propagate wrapped object(s) for binding '%s' to all required WECs - %w", binding.GetName(), err)
	}

	// all objects that appear in the desired state were handled. need to remove wrapped objects that are not part of the desired state
	for _, wrappedObject := range currentWrappedObjectList.Items { // objects left in currentWrappedObjectList.Items have to be deleted
		if err := c.deleteWrappedObject(ctx, wrappedObject.GetNamespace(), wrappedObject.GetName()); err != nil {
			return fmt.Errorf("failed to delete wrapped object from destinations that were removed from desired state - %w", err)
		}
	}

	return nil
}

func (c *genericTransportController) getObjectsFromWDS(ctx context.Context, binding *v1alpha1.Binding) ([]*unstructured.Unstructured, error) {
	objectsToPropagate := make([]*unstructured.Unstructured, 0)
	// add cluster-scoped objects to the 'objectsToPropagate' slice
	for _, clusterScopedObject := range binding.Spec.Workload.ClusterScope {
		gvr := schema.GroupVersionResource(clusterScopedObject.GroupVersionResource)
		object, err := c.wdsDynamicClient.Resource(gvr).Get(ctx, clusterScopedObject.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get required cluster-scoped object '%s' with gvr %s from WDS - %w", clusterScopedObject.Name, gvr, err)
		}
		objectsToPropagate = append(objectsToPropagate, cleanObject(object))
	}
	// add namespace-scoped objects to the 'objectsToPropagate' slice
	for _, namespaceScopedObject := range binding.Spec.Workload.NamespaceScope {
		gvr := schema.GroupVersionResource(namespaceScopedObject.GroupVersionResource)
		object, err := c.wdsDynamicClient.Resource(gvr).Namespace(namespaceScopedObject.Namespace).Get(ctx, namespaceScopedObject.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get required namespace-scoped object '%s' in namespace '%s' with gvr '%s' from WDS - %w", namespaceScopedObject.Name,
				namespaceScopedObject.Namespace, gvr, err)
		}
		objectsToPropagate = append(objectsToPropagate, cleanObject(object))
	}

	return objectsToPropagate, nil
}

// computeDestToWrappedObjects returns the following three things.
//   - the destToWrappedObject function. This maps a destination to the customized wrapped object
//     that should go to that destination. This func also returns a `bool` that is false when
//     the function has no answer for the given destination.
//   - the slice of strings describing user errors in the Binding.
//   - an error if something transient went wrong.
func (c *genericTransportController) computeDestToWrappedObjects(ctx context.Context, binding *v1alpha1.Binding) (func(v1alpha1.Destination) (*unstructured.Unstructured, bool), []string, error) {
	objectsToPropagate, err := c.getObjectsFromWDS(ctx, binding)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get objects to propagate to WECs from Binding object '%s' - %w", binding.GetName(), err)
	}

	if len(objectsToPropagate) == 0 {
		return nil, nil, nil // if no objects were found in the workload section, return nil so that we don't distribute an empty wrapped object.
	}

	destToCustomizedObjects, bindingErrors := c.computeDestToCustomizedObjects(objectsToPropagate, binding)

	// This will be constant if no object needed customization, otherwise a map's get func
	var destToWrappedObject func(v1alpha1.Destination) (*unstructured.Unstructured, bool)

	if destToCustomizedObjects != nil {
		asMap := map[v1alpha1.Destination]*unstructured.Unstructured{}
		for dest, objects := range destToCustomizedObjects {
			wrappedObject, err := c.wrap(objects, binding)
			if err != nil {
				return nil, nil, fmt.Errorf("failure wrapping for destination %q: %w", binding.Name, err)
			}
			asMap[dest] = wrappedObject
		}
		destToWrappedObject = a.PrimitiveMapGet(asMap)
	} else {
		wrappedObject, err := c.wrap(objectsToPropagate, binding)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to convert wrapped object to unstructured - %w", err)
		}
		destToWrappedObject = func(v1alpha1.Destination) (*unstructured.Unstructured, bool) { return wrappedObject, true }
	}

	return destToWrappedObject, bindingErrors, nil
}

// computeDestToCustomizedObjects returns the following two things.
//   - a map from destination to slice of customized workload objects.
//     This map will be nil if customization is not needed for the given slice of objects.
//   - the slice of strings containing the user errors found in the given Binding.
//
// This func also updates c.bindingSensitiveDestinations for the given Binding.
func (c *genericTransportController) computeDestToCustomizedObjects(objectsToPropagate []*unstructured.Unstructured, binding *v1alpha1.Binding) (map[v1alpha1.Destination][]*unstructured.Unstructured, []string) {
	// This will become non-nil if any object to propagate needs customization
	var destToCustomizedObjects map[v1alpha1.Destination][]*unstructured.Unstructured

	bindingErrors := []string{}

	// Look through the objects to propagate to see if any needs customization.
	// If any needs customization then catch up destToCustomizedObjects and proceed from there.
	for objIdx, objToPropagate := range objectsToPropagate {
		objAnnotations := objToPropagate.GetAnnotations()
		objRequestsExpansion := objAnnotations[v1alpha1.TemplateExpansionAnnotationKey] == "true"
		customizeThisObject := false
		reportedSomeErrors := false
		objRefStr := util.RefToRuntimeObj(objToPropagate).String()
		for destIdx, dest := range binding.Spec.Destinations {
			objC := objToPropagate
			var customizationErrors []string
			if objRequestsExpansion && (destIdx == 0 || customizeThisObject) {
				defs := c.getPropertiesForDestination(binding.Name, dest)
				// customizeThisObject does not vary with destination, for a given objToPropagate
				objC, customizationErrors, customizeThisObject = c.customizeForDestination(objToPropagate, dest.ClusterId+"/"+objRefStr, defs)
				if len(customizationErrors) != 0 && !reportedSomeErrors {
					// Let's not overwhelm the user, only report errors from the first troubled destination
					reportedSomeErrors = true
					bindingErrors = append(bindingErrors, customizationErrors...)
				}
				if !customizeThisObject {
					objC = objToPropagate
				}
			}
			if customizeThisObject && destToCustomizedObjects == nil {
				destToCustomizedObjects = map[v1alpha1.Destination][]*unstructured.Unstructured{}
				for _, dest := range binding.Spec.Destinations {
					destToCustomizedObjects[dest] = a.SliceCopy(objectsToPropagate[:objIdx])
				}
			}
			if destToCustomizedObjects != nil {
				customizedObjectsSoFar := destToCustomizedObjects[dest]
				customizedObjectsSoFar = append(customizedObjectsSoFar, objC)
				destToCustomizedObjects[dest] = customizedObjectsSoFar
			}
		}
	}
	// update the index in c.bindingSensitiveDestinations
	var cares sets.Set[v1alpha1.Destination]
	if destToCustomizedObjects != nil {
		cares = sets.New(binding.Spec.Destinations...)
	} else {
		cares = sets.New[v1alpha1.Destination]()
	}
	c.setBindingSensitivities(binding.Name, cares) // forget about now-irrelevant destinations

	return destToCustomizedObjects, bindingErrors
}

func (c *genericTransportController) wrap(objectsToPropagate []*unstructured.Unstructured, binding *v1alpha1.Binding) (*unstructured.Unstructured, error) {
	wrappedObject, err := convertObjectToUnstructured(c.transport.WrapObjects(objectsToPropagate))
	if err != nil {
		return nil, fmt.Errorf("failed to convert wrapped object to unstructured - %w", err)
	}
	// wrapped object name is (Binding.GetName()-WdsName).
	// pay attention - we cannot use the Binding object name, cause we might have duplicate names coming from different WDS spaces.
	// we add WdsName to the object name to assure name uniqueness,
	// in order to easily get the origin Binding object name and wds, we add it as an annotations.
	wrappedObject.SetName(fmt.Sprintf("%s-%s", binding.GetName(), c.wdsName))
	setLabel(wrappedObject, originOwnerReferenceLabel, binding.GetName())
	setLabel(wrappedObject, originWdsLabel, c.wdsName)
	setAnnotation(wrappedObject, originOwnerGenerationAnnotation, binding.GetGeneration())
	return wrappedObject, nil
}

// getPropertiesForDestination returns the properties to use for the given destination and notes
// that the given binding is sensitive to the fact that the destination has those properties.
func (c *genericTransportController) getPropertiesForDestination(bindingName string, dest v1alpha1.Destination) clusterProperties {
	c.propsMutex.Lock()
	defer c.propsMutex.Unlock()
	dests := c.bindingSensitiveDestinations[bindingName]
	if dests == nil {
		dests = sets.New[v1alpha1.Destination](dest)
		c.bindingSensitiveDestinations[bindingName] = dests
	} else {
		dests.Insert(dest)
	}
	props, have := c.destinationProperties[dest]
	if have {
		return props
	}
	props = c.collectPropertiesForDestination(c.logger.WithValues("forBinding", bindingName), dest.ClusterId)
	c.destinationProperties[dest] = props
	c.logger.V(4).Info("getPropertiesForDestination", "bindingName", bindingName, "dest", dest, "props", props)
	return props
}

// collectPropertiesForDestination computes the properties for the given destination
func (c *genericTransportController) collectPropertiesForDestination(logger logr.Logger, invName string) clusterProperties {
	props := clusterProperties{"clusterName": invName}
	collectProperty := func(key, val string) bool {
		props[key] = val
		return true
	}
	invObj, err := c.inventoryLister.Get(invName)
	if err == nil && invObj != nil {
		enumeratePropertiesInMapStringToString(invObj.Labels)(collectProperty)
		enumeratePropertiesInMapStringToString(invObj.Annotations)(collectProperty)
	} else if err != nil && !errors.IsNotFound(err) { // listers do not fail
		logger.Error(err, "Inconceivable failure to fetch inventory object", "dest", invName)
	}
	propCfgMap, err := c.propCfgMapLister.Get(invName)
	if err == nil && propCfgMap != nil {
		enumeratePropsInConfigMap(propCfgMap)(collectProperty)
	} else if err != nil && !errors.IsNotFound(err) { // listers do not fail
		logger.Error(err, "Inconceivable failure to fetch property ConfigMap", "dest", invName)
	}
	return props
}

func enumeratePropsInConfigMap(propCfgMap *corev1.ConfigMap) func(yield func(key, val string) bool) {
	return func(yield func(key, val string) bool) {
		if propCfgMap == nil {
			return
		}
		enumeratePropertiesInMapStringToString(propCfgMap.Data)(yield)
		for key, val := range propCfgMap.BinaryData {
			if token.IsIdentifier(key) && !yield(key, string(val)) {
				return
			}
		}
	}
}

func enumeratePropertiesInMapStringToString(theMap map[string]string) func(yield func(key, val string) bool) {
	return func(yield func(key, val string) bool) {
		for key, val := range theMap {
			if token.IsIdentifier(key) && !yield(key, val) {
				return
			}
		}
	}
}

func (c *genericTransportController) setBindingSensitivities(bindingName string, dests sets.Set[v1alpha1.Destination]) {
	c.propsMutex.Lock()
	defer c.propsMutex.Unlock()
	if dests == nil {
		delete(c.bindingSensitiveDestinations, bindingName)
	} else {
		c.bindingSensitiveDestinations[bindingName] = dests
	}
}

func (c *genericTransportController) handlePropertiesEvent(triggerObj interface{}, event string) {
	triggerObjM := triggerObj.(metav1.Object)
	invName := triggerObjM.GetName()
	c.logger.V(4).Info("Enqueuing a reconsideration of properties of inventory item due to informer event", "name", invName, "objType", fmt.Sprintf("%T", triggerObj), "resourceVersion", triggerObjM.GetResourceVersion(), "event", event)
	ref := recollectProperties(invName)
	c.workqueue.Add(ref)
}

// customizeForDestination customizes the given object for the given destination,
// if any customization is called for. The returned boolean indicates whether
// any customization was called for.
func (c *genericTransportController) customizeForDestination(object *unstructured.Unstructured, destination string, properties clusterProperties) (*unstructured.Unstructured, []string, bool) {
	objectCopy := object.DeepCopy()
	objectData := objectCopy.UnstructuredContent()
	objectDataExpanded, wantedChange, errs := customize.ExpandTemplates(destination, objectData, properties)
	if wantedChange {
		objectData = objectDataExpanded.(map[string]any)
		objectCopy.SetUnstructuredContent(objectData)
		return objectCopy, errs, true
	}
	return object, nil, false
}

func (c *genericTransportController) propagateWrappedObjectToClusters(ctx context.Context, destToDesiredWrappedObject func(v1alpha1.Destination) (*unstructured.Unstructured, bool),
	currentWrappedObjectList *unstructured.UnstructuredList, destinations []v1alpha1.Destination, broken bool) error {
	// if the desired wrapped object is nil, that means we should not propagate this object.
	// this may happen when the workload section is empty.
	// this is not an error state but a valid scenario.
	// return without propagating, the delete section will remove existing instances of the wrapped object from all current destinations.
	if destToDesiredWrappedObject == nil {
		return nil // this is not considered an error.
	}

	for _, destination := range destinations {
		currentWrappedObject := c.popWrappedObjectByNamespace(currentWrappedObjectList, destination.ClusterId)
		if broken {
			continue
		}
		desiredWrappedObject, _ := destToDesiredWrappedObject(destination)
		if currentWrappedObject != nil && apiequality.Semantic.DeepEqual(currentWrappedObject.UnstructuredContent(), desiredWrappedObject.UnstructuredContent()) {
			continue // current wrapped object is already in the desired state
		}
		// othereise, need to create or update the wrapped object
		if err := c.createOrUpdateWrappedObject(ctx, destination.ClusterId, desiredWrappedObject); err != nil {
			return fmt.Errorf("failed to propagate wrapped object to cluster mailbox namespace '%s' - %w", destination.ClusterId, err)
		}
	}

	return nil
}

// pops wrapped object by namespace from the list and returns the requested wrapped object.
// if the object is not found, list remains the same and nil is returned.
// since the order of items in the list is not important, the implementation is efficient and was done as follows:
// the functions goes over the list, if the requested object is found, it's replaced with the last object in the list.
// then the function removes the last object in the list and returns the object.
// in worst case where object is not found, it will go over all items in the list.
func (c *genericTransportController) popWrappedObjectByNamespace(list *unstructured.UnstructuredList, namespace string) *unstructured.Unstructured {
	length := len(list.Items)
	for i := 0; i < length; i++ {
		if list.Items[i].GetNamespace() == namespace {
			requiredObject := list.Items[i]
			list.Items[i] = list.Items[length-1]
			list.Items = list.Items[:length-1]
			return &requiredObject
		}
	}

	return nil
}

func (c *genericTransportController) createOrUpdateWrappedObject(ctx context.Context, namespace string, wrappedObject *unstructured.Unstructured) error {
	existingWrappedObject, err := c.transportClient.Resource(c.wrappedObjectGVR).Namespace(namespace).Get(ctx, wrappedObject.GetName(), metav1.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) { // if object is not there, we need to create it. otherwise report an error.
			return fmt.Errorf("failed to create wrapped object '%s' in destination WEC with namespace '%s' - %w", wrappedObject.GetName(), namespace, err)
		}
		// object not found when using get, create it
		wrappedObject.SetResourceVersion("") // must be unset for this destination
		wrappedObject2, err := c.transportClient.Resource(c.wrappedObjectGVR).Namespace(namespace).Create(ctx, wrappedObject, metav1.CreateOptions{
			FieldManager: ControllerName,
		})
		logger := klog.FromContext(ctx)
		if err != nil {
			return fmt.Errorf("failed to create wrapped object '%s' in destination WEC mailbox namespace '%s' - %w", wrappedObject.GetName(), namespace, err)
		}
		if hi := logger.V(4); hi.Enabled() {
			hi.Info("Created wrapped object in ITS", "namespace", namespace, "objectName", wrappedObject.GetName(), "wrappedObject", wrappedObject2)
		} else {
			logger.V(3).Info("Created wrapped object in ITS", "namespace", namespace, "objectName", wrappedObject.GetName(), "resourceVersion", wrappedObject2.GetResourceVersion())
		}
		return nil
	}
	// // if we reached here object already exists, try update object
	wrappedObject.SetResourceVersion(existingWrappedObject.GetResourceVersion())
	_, err = c.transportClient.Resource(c.wrappedObjectGVR).Namespace(namespace).Update(ctx, wrappedObject, metav1.UpdateOptions{
		FieldManager: ControllerName,
	})
	if err != nil {
		return fmt.Errorf("failed to update wrapped object '%s' in destination WEC mailbox namespace '%s' - %w", wrappedObject.GetName(), namespace, err)
	}
	klog.FromContext(ctx).V(3).Info("Updated wrapped object in ITS", "namespace", namespace, "objectName", wrappedObject.GetName(), "wrappedObject", wrappedObject)

	return nil
}

// updateObjectFunc is a function that updates the given object.
// returns the updated object (if it was updated) or the object as is if it wasn't, and true if object was updated, or false otherwise.
type updateObjectFunc func(*v1alpha1.Binding) (*v1alpha1.Binding, bool)

func (c *genericTransportController) updateBinding(ctx context.Context, binding *v1alpha1.Binding, updateObjectFunc updateObjectFunc) error {
	updatedBinding, isUpdated := updateObjectFunc(binding) // returns an indication if object was updated or not.
	if !isUpdated {
		return nil // if object was not updated, no need to update in API server, return.
	}
	logger := klog.FromContext(ctx)
	binding2, err := c.bindingClient.Update(ctx, updatedBinding, metav1.UpdateOptions{
		FieldManager: ControllerName,
	})
	if err != nil {
		return fmt.Errorf("failed to update Binding object '%s' in WDS - %w", binding.GetName(), err)
	}
	logger.V(3).Info("Updated Binding", "name", binding.Name, "resourceVersion", binding2.ResourceVersion)
	return nil
}

// addFinalizer accepts Binding object and adds the provided finalizer if not present.
// It returns the updated (or not) Binding and an indication of whether it updated the object's list of finalizers.
func addFinalizer(binding *v1alpha1.Binding, finalizer string) (*v1alpha1.Binding, bool) {
	finalizers := binding.GetFinalizers()
	for _, item := range finalizers {
		if item == finalizer { // finalizer already exists, no need to add
			return binding, false
		}
	}
	// if we reached here, finalizer has to be added to the Binding object.
	// objects returned from a BindingLister must be treated as read-only.
	// Therefore, create a deep copy before updating the object.
	updatedBinding := binding.DeepCopy()
	updatedBinding.SetFinalizers(append(finalizers, finalizer))
	return updatedBinding, true
}

// removeFinalizer accepts Binding object and removes the provided finalizer if present.
// It returns the updated (or not) Binding and an indication of whether it updated the object's list of finalizers.
func removeFinalizer(binding *v1alpha1.Binding, finalizer string) (*v1alpha1.Binding, bool) {
	finalizersList := binding.GetFinalizers()
	length := len(finalizersList)

	index := 0
	for i := 0; i < length; i++ {
		if finalizersList[i] == finalizer {
			continue
		}
		finalizersList[index] = finalizersList[i]
		index++
	}
	if length == index { // finalizer wasn't found, no need to remove
		return binding, false
	}
	// otherwise, finalizer was found and has to be removed.
	// objects returned from a BindingLister must be treated as read-only.
	// Therefore, create a deep copy before updating the object.
	updatedBinding := binding.DeepCopy()
	updatedBinding.SetFinalizers(finalizersList[:index])
	return updatedBinding, true
}

// setAnnotation sets metadata annotation on the given object.
func setAnnotation(object metav1.Object, key string, value any) {
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	annotations[key] = fmt.Sprint(value)

	object.SetAnnotations(annotations)
}

// setLabel sets metadata label on the given object.
func setLabel(object metav1.Object, key string, value any) {
	labels := object.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	labels[key] = fmt.Sprint(value)

	object.SetLabels(labels)
}

// cleanObject is a function to clean object before adding it to a wrapped object. these fields shouldn't be propagated to WEC.
func cleanObject(object *unstructured.Unstructured) *unstructured.Unstructured {
	objectCopy := object.DeepCopy() // don't modify object directly. create a copy before zeroing fields
	objectCopy.SetManagedFields(nil)
	objectCopy.SetFinalizers(nil)
	objectCopy.SetGeneration(0)
	objectCopy.SetOwnerReferences(nil)
	objectCopy.SetSelfLink("")
	objectCopy.SetResourceVersion("")
	objectCopy.SetUID("")
	objectCopy.SetGenerateName("")

	annotations := objectCopy.GetAnnotations()
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	objectCopy.SetAnnotations(annotations)

	// remove the status field.
	unstructured.RemoveNestedField(objectCopy.Object, "status")

	// clean fields specific to the concrete object.
	objectsFilter.CleanObjectSpecifics(objectCopy)

	return objectCopy
}
