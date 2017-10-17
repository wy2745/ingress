/*
Copyright 2017 The Kubernetes Authors.

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

	"github.com/golang/glog"

	"k8s.io/client-go/listers/core/v1"
	apiv1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"
	//kube_api "k8s.io/client-go/pkg/api/v1"
	fcache "k8s.io/client-go/tools/cache/testing"

	"github.com/wy2745/ingress/core/pkg/ingress/annotations/class"
	"github.com/wy2745/ingress/core/pkg/ingress/annotations/parser"
	"time"
	"net/url"
	"k8s.io/heapster/metrics/core"
	"k8s.io/heapster/metrics/processors"
)

//添加辅助函数
type StoreToIngressLister struct {
	cache.Store
}
func (ic *GenericController) getIngressesForService(svc *apiv1.Service) []extensions.Ingress {
	ings, err := ic.listers.Ingress.GetServiceIngress(svc)
	if err != nil {
		glog.V(3).Infof("ignoring service %v: %v", svc.Name, err)
		return nil
	}
	return ings
}

func (ic *GenericController) enqueueIngressForService(svc *apiv1.Service) {
	ings := ic.getIngressesForService(svc)
	for _, ing := range ings {
		if !!class.IsValid(&ing, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
			continue
		}
		ic.syncQueue.Enqueue(&ing)
	}
}

func (ic *GenericController) createListers(disableNodeLister bool) {
	// from here to the end of the method all the code is just boilerplate
	// required to watch Ingress, Secrets, ConfigMaps and Endoints.
	// This is used to detect new content, updates or removals and act accordingly
	ingEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addIng := obj.(*extensions.Ingress)
			if !class.IsValid(addIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
				a, _ := parser.GetStringAnnotation(class.IngressKey, addIng)
				glog.Infof("ignoring add for ingress %v based on annotation %v with value %v", addIng.Name, class.IngressKey, a)
				return
			}
			ic.recorder.Eventf(addIng, apiv1.EventTypeNormal, "CREATE", fmt.Sprintf("Ingress %s/%s", addIng.Namespace, addIng.Name))

			if ic.isInitialSyncDone() {
				ic.syncQueue.Enqueue(obj)
			}
		},
		DeleteFunc: func(obj interface{}) {
			delIng, ok := obj.(*extensions.Ingress)
			if !ok {
				// If we reached here it means the ingress was deleted but its final state is unrecorded.
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Errorf("couldn't get object from tombstone %#v", obj)
					return
				}
				delIng, ok = tombstone.Obj.(*extensions.Ingress)
				if !ok {
					glog.Errorf("Tombstone contained object that is not an Ingress: %#v", obj)
					return
				}
			}
			if !class.IsValid(delIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
				glog.Infof("ignoring delete for ingress %v based on annotation %v", delIng.Name, class.IngressKey)
				return
			}
			ic.recorder.Eventf(delIng, apiv1.EventTypeNormal, "DELETE", fmt.Sprintf("Ingress %s/%s", delIng.Namespace, delIng.Name))
			ic.syncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			oldIng := old.(*extensions.Ingress)
			curIng := cur.(*extensions.Ingress)
			validOld := class.IsValid(oldIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass)
			validCur := class.IsValid(curIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass)
			if !validOld && validCur {
				glog.Infof("creating ingress %v based on annotation %v", curIng.Name, class.IngressKey)
				ic.recorder.Eventf(curIng, apiv1.EventTypeNormal, "CREATE", fmt.Sprintf("Ingress %s/%s", curIng.Namespace, curIng.Name))
			} else if validOld && !validCur {
				glog.Infof("removing ingress %v based on annotation %v", curIng.Name, class.IngressKey)
				ic.recorder.Eventf(curIng, apiv1.EventTypeNormal, "DELETE", fmt.Sprintf("Ingress %s/%s", curIng.Namespace, curIng.Name))
			} else if validCur && !reflect.DeepEqual(old, cur) {
				ic.recorder.Eventf(curIng, apiv1.EventTypeNormal, "UPDATE", fmt.Sprintf("Ingress %s/%s", curIng.Namespace, curIng.Name))
			}

			ic.syncQueue.Enqueue(cur)
		},
	}

	svcHandlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addSvc := obj.(*apiv1.Service)
			glog.Infof("Adding service: %v", addSvc.Name)
			ic.recorder.Eventf(addSvc, apiv1.EventTypeNormal, "CREATE", fmt.Sprintf("Service %s/%s", addSvc.Namespace, addSvc.Name))
			ic.enqueueIngressForService(addSvc)
		},
		DeleteFunc: func(obj interface{}) {
			remSvc, isSvc := obj.(*apiv1.Service)
			if !isSvc {
				deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Infof("Error received unexpected object: %v", obj)
					return
				}
				remSvc, ok = deletedState.Obj.(*apiv1.Service)
				if !ok {
					glog.Infof("Error DeletedFinalStateUnknown contained non-Service object: %v", deletedState.Obj)
					return
				}
			}
			glog.Infof("Removing service: %v", remSvc.Name)
			ic.recorder.Eventf(remSvc, apiv1.EventTypeNormal, "DELETE", fmt.Sprintf("Ingress %s/%s", remSvc.Namespace, remSvc.Name))
			ic.enqueueIngressForService(remSvc)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				glog.Infof("Service %v changed, syncing",
					cur.(*apiv1.Service).Name)
				ic.recorder.Eventf(cur.(*apiv1.Service), apiv1.EventTypeNormal, "UPDATE", fmt.Sprintf("Ingress %s/%s", cur.(*apiv1.Service).Namespace, cur.(*apiv1.Service).Name))
				ic.enqueueIngressForService(cur.(*apiv1.Service))
			}
		},
	}

	secrEventHandler := cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				sec := cur.(*apiv1.Secret)
				key := fmt.Sprintf("%v/%v", sec.Namespace, sec.Name)
				ic.syncSecret(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			sec, ok := obj.(*apiv1.Secret)
			if !ok {
				// If we reached here it means the secret was deleted but its final state is unrecorded.
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Errorf("couldn't get object from tombstone %#v", obj)
					return
				}
				sec, ok = tombstone.Obj.(*apiv1.Secret)
				if !ok {
					glog.Errorf("Tombstone contained object that is not a Secret: %#v", obj)
					return
				}
			}
			key := fmt.Sprintf("%v/%v", sec.Namespace, sec.Name)
			ic.sslCertTracker.DeleteAll(key)
		},
	}

	eventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ic.syncQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			ic.syncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			oep := old.(*apiv1.Endpoints)
			ocur := cur.(*apiv1.Endpoints)
			if !reflect.DeepEqual(ocur.Subsets, oep.Subsets) {
				ic.syncQueue.Enqueue(cur)
			}
		},
	}

	mapEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			upCmap := obj.(*apiv1.ConfigMap)
			mapKey := fmt.Sprintf("%s/%s", upCmap.Namespace, upCmap.Name)
			if mapKey == ic.cfg.ConfigMapName {
				glog.V(2).Infof("adding configmap %v to backend", mapKey)
				ic.cfg.Backend.SetConfig(upCmap)
				ic.setForceReload(true)
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				upCmap := cur.(*apiv1.ConfigMap)
				mapKey := fmt.Sprintf("%s/%s", upCmap.Namespace, upCmap.Name)
				if mapKey == ic.cfg.ConfigMapName {
					glog.V(2).Infof("updating configmap backend (%v)", mapKey)
					ic.cfg.Backend.SetConfig(upCmap)
					ic.setForceReload(true)
				}
				// updates to configuration configmaps can trigger an update
				if mapKey == ic.cfg.ConfigMapName || mapKey == ic.cfg.TCPConfigMapName || mapKey == ic.cfg.UDPConfigMapName {
					ic.recorder.Eventf(upCmap, apiv1.EventTypeNormal, "UPDATE", fmt.Sprintf("ConfigMap %v", mapKey))
					ic.syncQueue.Enqueue(cur)
				}
			}
		},
	}



	watchNs := apiv1.NamespaceAll
	if ic.cfg.ForceNamespaceIsolation && ic.cfg.Namespace != apiv1.NamespaceAll {
		watchNs = ic.cfg.Namespace
	}

	//更新间隔时间在这里生效了，每个lister都是在固定的时间间隔内获取数据

	ic.listers.Ingress.Store, ic.ingController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.ExtensionsV1beta1().RESTClient(), "ingresses", ic.cfg.Namespace, fields.Everything()),
		&extensions.Ingress{}, ic.cfg.ResyncPeriod, ingEventHandler)

	ic.listers.Endpoint.Store, ic.endpController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "endpoints", ic.cfg.Namespace, fields.Everything()),
		&apiv1.Endpoints{}, ic.cfg.ResyncPeriod, eventHandler)

	ic.listers.Secret.Store, ic.secrController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "secrets", watchNs, fields.Everything()),
		&apiv1.Secret{}, ic.cfg.ResyncPeriod, secrEventHandler)

	ic.listers.ConfigMap.Store, ic.mapController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "configmaps", watchNs, fields.Everything()),
		&apiv1.ConfigMap{}, ic.cfg.ResyncPeriod, mapEventHandler)

	//ic.listers.Service.Store, ic.svcController = cache.NewInformer(
	//	cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "services", ic.cfg.Namespace, fields.Everything()),
	//	&apiv1.Service{}, ic.cfg.ResyncPeriod, cache.ResourceEventHandlerFuncs{})
	ic.listers.Service.Store, ic.svcController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "services", ic.cfg.Namespace, fields.Everything()),
		&apiv1.Service{}, ic.cfg.ResyncPeriod, svcHandlers)

	var nodeListerWatcher cache.ListerWatcher
	if disableNodeLister {
		nodeListerWatcher = fcache.NewFakeControllerSource()
	} else {
		nodeListerWatcher = cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "nodes", apiv1.NamespaceAll, fields.Everything())
	}

	ic.listers.Node.Store, ic.nodeController = cache.NewInformer(
		nodeListerWatcher,
		&apiv1.Node{}, ic.cfg.ResyncPeriod, cache.ResourceEventHandlerFuncs{})
	ic.listers.NodeReflector = cache.NewReflector(nodeListerWatcher, &apiv1.Node{}, ic.listers.Node.Store, time.Hour)
	ic.listers.NodeReflector.RunV2()

	lw := cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "pods", apiv1.NamespaceAll, fields.Everything())
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	ic.listers.Pod = v1.NewPodLister(store)
	ic.listers.PodReflector = cache.NewReflector(lw, &apiv1.Pod{}, store, time.Hour)
	ic.listers.PodReflector.RunV2()


}
