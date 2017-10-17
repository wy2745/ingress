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

package store

import (
	"fmt"

	apiv1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
)

// IngressLister makes a Store that lists Ingress.
type IngressLister struct {
	cache.Store
}

//为IngressLister添加一个辅助函数
// GetServiceIngress gets all the Ingress' that have rules pointing to a service.
// Note that this ignores services without the right nodePorts.
func (s *IngressLister) GetServiceIngress(svc *apiv1.Service) (ings []extensions.Ingress, err error) {
	for _, m := range s.Store.List() {
		ing := *m.(*extensions.Ingress)
		if ing.Namespace != svc.Namespace {
			continue
		}
		if ing.Spec.Backend != nil {
			if ing.Spec.Backend.ServiceName == svc.Name {
				ings = append(ings, ing)
			}
		}
		for _, rules := range ing.Spec.Rules {
			if rules.IngressRuleValue.HTTP == nil {
				continue
			}
			for _, p := range rules.IngressRuleValue.HTTP.Paths {
				if p.Backend.ServiceName == svc.Name {
					ings = append(ings, ing)
				}
			}
		}
	}
	if len(ings) == 0 {
		err = fmt.Errorf("No ingress for service %v", svc.Name)
	}
	return
}

// SecretsLister makes a Store that lists Secrets.
type SecretLister struct {
	cache.Store
}

// GetByName searches for a secret in the local secrets Store
func (sl *SecretLister) GetByName(name string) (*apiv1.Secret, error) {
	s, exists, err := sl.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("secret %v was not found", name)
	}
	return s.(*apiv1.Secret), nil
}

// ConfigMapLister makes a Store that lists Configmaps.
type ConfigMapLister struct {
	cache.Store
}

// GetByName searches for a configmap in the local configmaps Store
func (cml *ConfigMapLister) GetByName(name string) (*apiv1.ConfigMap, error) {
	s, exists, err := cml.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("configmap %v was not found", name)
	}
	return s.(*apiv1.ConfigMap), nil
}

// ServiceLister makes a Store that lists Services.
type ServiceLister struct {
	cache.Store
}

// GetByName searches for a service in the local secrets Store
func (sl *ServiceLister) GetByName(name string) (*apiv1.Service, error) {
	s, exists, err := sl.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("service %v was not found", name)
	}
	return s.(*apiv1.Service), nil
}

// NodeLister makes a Store that lists Nodes.
type NodeLister struct {
	cache.Store
}

// List lists all Nodes in the indexer.
func (s *NodeLister) List(selector labels.Selector) (ret []*apiv1.Node, err error) {
	err = cache.ListAll(s.Store, selector, func(m interface{}) {
		ret = append(ret, m.(*apiv1.Node))
	})
	return ret, err
}

// Get retrieves the Node from the index for a given name.
func (s *NodeLister) Get(name string) (*apiv1.Node, error) {
	key := &apiv1.Node{ObjectMeta: meta_v1.ObjectMeta{Name: name}}
	obj, exists, err := s.Store.Get(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(apiv1.Resource("node"), name)
	}
	return obj.(*apiv1.Node), nil
}

// EndpointLister makes a Store that lists Endpoints.
type EndpointLister struct {
	cache.Store
}

// GetServiceEndpoints returns the endpoints of a service, matched on service name.
func (s *EndpointLister) GetServiceEndpoints(svc *apiv1.Service) (ep apiv1.Endpoints, err error) {
	for _, m := range s.Store.List() {
		ep = *m.(*apiv1.Endpoints)
		if svc.Name == ep.Name && svc.Namespace == ep.Namespace {
			return ep, nil
		}
	}
	err = fmt.Errorf("could not find endpoints for service: %v", svc.Name)
	return
}
