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
	"github.com/golang/glog"

	"github.com/imdario/mergo"

	api "k8s.io/api/core/v1"

	"github.com/wy2745/ingress/core/pkg/ingress"
)

// DeniedKeyName name of the key that contains the reason to deny a location
const DeniedKeyName = "Denied"

// newUpstream creates an upstream without servers.
func newUpstream(name string) *ingress.Backend {
	return &ingress.Backend{
		Name:      name,
		Endpoints: []ingress.Endpoint{},
		Service:   &api.Service{},
		SessionAffinity: ingress.SessionAffinityConfig{
			CookieSessionAffinity: ingress.CookieSessionAffinity{
				Locations: make(map[string][]string),
			},
		},
	}
}

func mergeLocationAnnotations(loc *ingress.Location, anns map[string]interface{}) {
	if _, ok := anns[DeniedKeyName]; ok {
		loc.Denied = anns[DeniedKeyName].(error)
	}
	delete(anns, DeniedKeyName)
	err := mergo.Map(loc, anns)
	if err != nil {
		glog.Errorf("unexpected error merging extracted annotations in location type: %v", err)
	}
}
