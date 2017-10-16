/*
Copyright 2015 The Kubernetes Authors.

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

package controller

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"

	apiv1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	informerv1 "k8s.io/client-go/informers/core/v1"
	informerv1beta1 "k8s.io/client-go/informers/extensions/v1beta1"
	"k8s.io/client-go/kubernetes"
	scheme "k8s.io/client-go/kubernetes/scheme"
	unversionedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/wy2745/ingress/controllers/gce/loadbalancers"
)

var (
	keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

	// DefaultClusterUID is the uid to use for clusters resources created by an
	// L7 controller created without specifying the --cluster-uid flag.
	DefaultClusterUID = ""

	// DefaultFirewallName is the name to user for firewall rules created
	// by an L7 controller when the --fireall-rule is not used.
	DefaultFirewallName = ""

	// Frequency to poll on local stores to sync.
	storeSyncPollPeriod = 5 * time.Second
)

// ControllerContext holds
type ControllerContext struct {
	IngressInformer cache.SharedIndexInformer
	ServiceInformer cache.SharedIndexInformer
	PodInformer     cache.SharedIndexInformer
	NodeInformer    cache.SharedIndexInformer
	// Stop is the stop channel shared among controllers
	StopCh chan struct{}
}

func NewControllerContext(kubeClient kubernetes.Interface, namespace string, resyncPeriod time.Duration) *ControllerContext {
	return &ControllerContext{
		IngressInformer: informerv1beta1.NewIngressInformer(kubeClient, namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		ServiceInformer: informerv1.NewServiceInformer(kubeClient, namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		PodInformer:     informerv1.NewPodInformer(kubeClient, namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		NodeInformer:    informerv1.NewNodeInformer(kubeClient, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		StopCh:          make(chan struct{}),
	}
}

func (ctx *ControllerContext) Start() {
	go ctx.IngressInformer.Run(ctx.StopCh)
	go ctx.ServiceInformer.Run(ctx.StopCh)
	go ctx.PodInformer.Run(ctx.StopCh)
	go ctx.NodeInformer.Run(ctx.StopCh)
}

// LoadBalancerController watches the kubernetes api and adds/removes services
// from the loadbalancer, via loadBalancerConfig.
type LoadBalancerController struct {
	client kubernetes.Interface

	ingressSynced cache.InformerSynced
	serviceSynced cache.InformerSynced
	podSynced     cache.InformerSynced
	nodeSynced    cache.InformerSynced
	ingLister     StoreToIngressLister
	nodeLister    StoreToNodeLister
	svcLister     StoreToServiceLister
	// Health checks are the readiness probes of containers on pods.
	podLister StoreToPodLister
	// TODO: Watch secrets
	CloudClusterManager *ClusterManager
	recorder            record.EventRecorder
	nodeQueue           *taskQueue
	ingQueue            *taskQueue
	tr                  *GCETranslator
	stopCh              chan struct{}
	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock sync.Mutex
	shutdown bool
	// tlsLoader loads secrets from the Kubernetes apiserver for Ingresses.
	tlsLoader tlsLoader
	// hasSynced returns true if all associated sub-controllers have synced.
	// Abstracted into a func for testing.
	hasSynced func() bool
}

// NewLoadBalancerController creates a controller for gce loadbalancers.
// - kubeClient: A kubernetes REST client.
// - clusterManager: A ClusterManager capable of creating all cloud resources
//	 required for L7 loadbalancing.
// - resyncPeriod: Watchers relist from the Kubernetes API server this often.
func NewLoadBalancerController(kubeClient kubernetes.Interface, ctx *ControllerContext, clusterManager *ClusterManager) (*LoadBalancerController, error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&unversionedcore.EventSinkImpl{
		Interface: kubeClient.Core().Events(""),
	})
	lbc := LoadBalancerController{
		client:              kubeClient,
		CloudClusterManager: clusterManager,
		stopCh:              ctx.StopCh,
		recorder: eventBroadcaster.NewRecorder(scheme.Scheme,
			apiv1.EventSource{Component: "loadbalancer-controller"}),
	}
	lbc.nodeQueue = NewTaskQueue(lbc.syncNodes)
	lbc.ingQueue = NewTaskQueue(lbc.sync)
	lbc.hasSynced = lbc.storesSynced

	lbc.ingressSynced = ctx.IngressInformer.HasSynced
	lbc.serviceSynced = ctx.ServiceInformer.HasSynced
	lbc.podSynced = ctx.PodInformer.HasSynced
	lbc.nodeSynced = ctx.NodeInformer.HasSynced

	lbc.ingLister.Store = ctx.IngressInformer.GetStore()
	lbc.svcLister.Indexer = ctx.ServiceInformer.GetIndexer()
	lbc.podLister.Indexer = ctx.PodInformer.GetIndexer()
	lbc.nodeLister.Indexer = ctx.NodeInformer.GetIndexer()
	// ingress event handler
	ctx.IngressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addIng := obj.(*extensions.Ingress)
			if !isGCEIngress(addIng) && !isGCEMultiClusterIngress(addIng) {
				glog.Infof("Ignoring add for ingress %v based on annotation %v", addIng.Name, ingressClassKey)
				return
			}
			lbc.recorder.Eventf(addIng, apiv1.EventTypeNormal, "ADD", fmt.Sprintf("%s/%s", addIng.Namespace, addIng.Name))
			lbc.ingQueue.enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			delIng := obj.(*extensions.Ingress)
			if !isGCEIngress(delIng) && !isGCEMultiClusterIngress(delIng) {
				glog.Infof("Ignoring delete for ingress %v based on annotation %v", delIng.Name, ingressClassKey)
				return
			}
			glog.Infof("Delete notification received for Ingress %v/%v", delIng.Namespace, delIng.Name)
			lbc.ingQueue.enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			curIng := cur.(*extensions.Ingress)
			if !isGCEIngress(curIng) && !isGCEMultiClusterIngress(curIng) {
				return
			}
			if !reflect.DeepEqual(old, cur) {
				glog.V(3).Infof("Ingress %v changed, syncing", curIng.Name)
			}
			lbc.ingQueue.enqueue(cur)
		},
	})

	// service event handler
	ctx.ServiceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: lbc.enqueueIngressForService,
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				lbc.enqueueIngressForService(cur)
			}
		},
		// Ingress deletes matter, service deletes don't.
	})

	// node event handler
	ctx.NodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    lbc.nodeQueue.enqueue,
		DeleteFunc: lbc.nodeQueue.enqueue,
		// Nodes are updated every 10s and we don't care, so no update handler.
	})

	lbc.tr = &GCETranslator{&lbc}
	lbc.tlsLoader = &apiServerTLSLoader{client: lbc.client}
	glog.V(3).Infof("Created new loadbalancer controller")

	return &lbc, nil
}

// enqueueIngressForService enqueues all the Ingress' for a Service.
func (lbc *LoadBalancerController) enqueueIngressForService(obj interface{}) {
	svc := obj.(*apiv1.Service)
	ings, err := lbc.ingLister.GetServiceIngress(svc)
	if err != nil {
		glog.V(5).Infof("ignoring service %v: %v", svc.Name, err)
		return
	}
	for _, ing := range ings {
		if !isGCEIngress(&ing) {
			continue
		}
		lbc.ingQueue.enqueue(&ing)
	}
}

// Run starts the loadbalancer controller.
func (lbc *LoadBalancerController) Run() {
	glog.Infof("Starting loadbalancer controller")
	go lbc.ingQueue.run(time.Second, lbc.stopCh)
	go lbc.nodeQueue.run(time.Second, lbc.stopCh)
	<-lbc.stopCh
	glog.Infof("Shutting down Loadbalancer Controller")
}

// Stop stops the loadbalancer controller. It also deletes cluster resources
// if deleteAll is true.
func (lbc *LoadBalancerController) Stop(deleteAll bool) error {
	// Stop is invoked from the http endpoint.
	lbc.stopLock.Lock()
	defer lbc.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !lbc.shutdown {
		close(lbc.stopCh)
		glog.Infof("Shutting down controller queues.")
		lbc.ingQueue.shutdown()
		lbc.nodeQueue.shutdown()
		lbc.shutdown = true
	}

	// Deleting shared cluster resources is idempotent.
	if deleteAll {
		glog.Infof("Shutting down cluster manager.")
		return lbc.CloudClusterManager.shutdown()
	}
	return nil
}

// storesSynced returns true if all the sub-controllers have finished their
// first sync with apiserver.
func (lbc *LoadBalancerController) storesSynced() bool {
	return (
	// wait for pods to sync so we don't allocate a default health check when
	// an endpoint has a readiness probe.
	lbc.podSynced() &&
		// wait for services so we don't thrash on backend creation.
		lbc.serviceSynced() &&
		// wait for nodes so we don't disconnect a backend from an instance
		// group just because we don't realize there are nodes in that zone.
		lbc.nodeSynced() &&
		// Wait for ingresses as a safety measure. We don't really need this.
		lbc.ingressSynced())
}

// sync manages Ingress create/updates/deletes.
func (lbc *LoadBalancerController) sync(key string) (err error) {
	if !lbc.hasSynced() {
		time.Sleep(storeSyncPollPeriod)
		return fmt.Errorf("waiting for stores to sync")
	}
	glog.V(3).Infof("Syncing %v", key)

	allIngresses, err := lbc.ingLister.ListAll()
	if err != nil {
		return err
	}
	gceIngresses, err := lbc.ingLister.ListGCEIngresses()
	if err != nil {
		return err
	}

	allNodePorts := lbc.tr.toNodePorts(&allIngresses)
	gceNodePorts := lbc.tr.toNodePorts(&gceIngresses)
	lbNames := lbc.ingLister.Store.ListKeys()
	lbs, err := lbc.toRuntimeInfo(gceIngresses)
	if err != nil {
		return err
	}
	nodeNames, err := lbc.getReadyNodeNames()
	if err != nil {
		return err
	}
	obj, ingExists, err := lbc.ingLister.Store.GetByKey(key)
	if err != nil {
		return err
	}

	// This performs a 2 phase checkpoint with the cloud:
	// * Phase 1 creates/verifies resources are as expected. At the end of a
	//   successful checkpoint we know that existing L7s are WAI, and the L7
	//   for the Ingress associated with "key" is ready for a UrlMap update.
	//   If this encounters an error, eg for quota reasons, we want to invoke
	//   Phase 2 right away and retry checkpointing.
	// * Phase 2 performs GC by refcounting shared resources. This needs to
	//   happen periodically whether or not stage 1 fails. At the end of a
	//   successful GC we know that there are no dangling cloud resources that
	//   don't have an associated Kubernetes Ingress/Service/Endpoint.

	var syncError error
	defer func() {
		if deferErr := lbc.CloudClusterManager.GC(lbNames, allNodePorts); deferErr != nil {
			err = fmt.Errorf("error during sync %v, error during GC %v", syncError, deferErr)
		}
		glog.V(3).Infof("Finished syncing %v", key)
	}()
	// Record any errors during sync and throw a single error at the end. This
	// allows us to free up associated cloud resources ASAP.
	igs, err := lbc.CloudClusterManager.Checkpoint(lbs, nodeNames, gceNodePorts, allNodePorts)
	if err != nil {
		// TODO: Implement proper backoff for the queue.
		eventMsg := "GCE"
		if ingExists {
			lbc.recorder.Eventf(obj.(*extensions.Ingress), apiv1.EventTypeWarning, eventMsg, err.Error())
		} else {
			err = fmt.Errorf("%v, error: %v", eventMsg, err)
		}
		syncError = err
	}

	if !ingExists {
		return syncError
	}
	ing := *obj.(*extensions.Ingress)
	if isGCEMultiClusterIngress(&ing) {
		// Add instance group names as annotation on the ingress.
		if ing.Annotations == nil {
			ing.Annotations = map[string]string{}
		}
		err = setInstanceGroupsAnnotation(ing.Annotations, igs)
		if err != nil {
			return err
		}
		if err := lbc.updateAnnotations(ing.Name, ing.Namespace, ing.Annotations); err != nil {
			return err
		}
		return nil
	}

	// Update the UrlMap of the single loadbalancer that came through the watch.
	l7, err := lbc.CloudClusterManager.l7Pool.Get(key)
	if err != nil {
		syncError = fmt.Errorf("%v, unable to get loadbalancer: %v", syncError, err)
		return syncError
	}

	if urlMap, err := lbc.tr.toURLMap(&ing); err != nil {
		syncError = fmt.Errorf("%v, convert to url map error %v", syncError, err)
	} else if err := l7.UpdateUrlMap(urlMap); err != nil {
		lbc.recorder.Eventf(&ing, apiv1.EventTypeWarning, "UrlMap", err.Error())
		syncError = fmt.Errorf("%v, update url map error: %v", syncError, err)
	} else if err := lbc.updateIngressStatus(l7, ing); err != nil {
		lbc.recorder.Eventf(&ing, apiv1.EventTypeWarning, "Status", err.Error())
		syncError = fmt.Errorf("%v, update ingress error: %v", syncError, err)
	}
	return syncError
}

// updateIngressStatus updates the IP and annotations of a loadbalancer.
// The annotations are parsed by kubectl describe.
func (lbc *LoadBalancerController) updateIngressStatus(l7 *loadbalancers.L7, ing extensions.Ingress) error {
	ingClient := lbc.client.Extensions().Ingresses(ing.Namespace)

	// Update IP through update/status endpoint
	ip := l7.GetIP()
	currIng, err := ingClient.Get(ing.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	currIng.Status = extensions.IngressStatus{
		LoadBalancer: apiv1.LoadBalancerStatus{
			Ingress: []apiv1.LoadBalancerIngress{
				{IP: ip},
			},
		},
	}
	if ip != "" {
		lbIPs := ing.Status.LoadBalancer.Ingress
		if len(lbIPs) == 0 || lbIPs[0].IP != ip {
			// TODO: If this update fails it's probably resource version related,
			// which means it's advantageous to retry right away vs requeuing.
			glog.Infof("Updating loadbalancer %v/%v with IP %v", ing.Namespace, ing.Name, ip)
			if _, err := ingClient.UpdateStatus(currIng); err != nil {
				return err
			}
			lbc.recorder.Eventf(currIng, apiv1.EventTypeNormal, "CREATE", "ip: %v", ip)
		}
	}
	annotations := loadbalancers.GetLBAnnotations(l7, currIng.Annotations, lbc.CloudClusterManager.backendPool)
	if err := lbc.updateAnnotations(ing.Name, ing.Namespace, annotations); err != nil {
		return err
	}
	return nil
}

func (lbc *LoadBalancerController) updateAnnotations(name, namespace string, annotations map[string]string) error {
	// Update annotations through /update endpoint
	ingClient := lbc.client.Extensions().Ingresses(namespace)
	currIng, err := ingClient.Get(name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currIng.Annotations, annotations) {
		glog.V(3).Infof("Updating annotations of %v/%v", namespace, name)
		currIng.Annotations = annotations
		if _, err := ingClient.Update(currIng); err != nil {
			return err
		}
	}
	return nil
}

// toRuntimeInfo returns L7RuntimeInfo for the given ingresses.
func (lbc *LoadBalancerController) toRuntimeInfo(ingList extensions.IngressList) (lbs []*loadbalancers.L7RuntimeInfo, err error) {
	for _, ing := range ingList.Items {
		k, err := keyFunc(&ing)
		if err != nil {
			glog.Warningf("Cannot get key for Ingress %v/%v: %v", ing.Namespace, ing.Name, err)
			continue
		}

		var tls *loadbalancers.TLSCerts

		annotations := ingAnnotations(ing.ObjectMeta.Annotations)
		// Load the TLS cert from the API Spec if it is not specified in the annotation.
		// TODO: enforce this with validation.
		if annotations.useNamedTLS() == "" {
			tls, err = lbc.tlsLoader.load(&ing)
			if err != nil {
				glog.Warningf("Cannot get certs for Ingress %v/%v: %v", ing.Namespace, ing.Name, err)
			}
		}

		lbs = append(lbs, &loadbalancers.L7RuntimeInfo{
			Name:         k,
			TLS:          tls,
			TLSName:      annotations.useNamedTLS(),
			AllowHTTP:    annotations.allowHTTP(),
			StaticIPName: annotations.staticIPName(),
		})
	}
	return lbs, nil
}

// syncNodes manages the syncing of kubernetes nodes to gce instance groups.
// The instancegroups are referenced by loadbalancer backends.
func (lbc *LoadBalancerController) syncNodes(key string) error {
	nodeNames, err := lbc.getReadyNodeNames()
	if err != nil {
		return err
	}
	if err := lbc.CloudClusterManager.instancePool.Sync(nodeNames); err != nil {
		return err
	}
	return nil
}

func getNodeReadyPredicate() listers.NodeConditionPredicate {
	return func(node *apiv1.Node) bool {
		for ix := range node.Status.Conditions {
			condition := &node.Status.Conditions[ix]
			if condition.Type == apiv1.NodeReady {
				return condition.Status == apiv1.ConditionTrue
			}
		}
		return false
	}
}

// getReadyNodeNames returns names of schedulable, ready nodes from the node lister.
func (lbc *LoadBalancerController) getReadyNodeNames() ([]string, error) {
	nodeNames := []string{}
	nodes, err := listers.NewNodeLister(lbc.nodeLister.Indexer).ListWithPredicate(getNodeReadyPredicate())
	if err != nil {
		return nodeNames, err
	}
	for _, n := range nodes {
		if n.Spec.Unschedulable {
			continue
		}
		nodeNames = append(nodeNames, n.Name)
	}
	return nodeNames, nil
}
