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

package template

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	text_template "text/template"

	"github.com/golang/glog"

	"github.com/pborman/uuid"

	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"
	"github.com/wy2745/ingress/controllers/nginx/pkg/config"
	"github.com/wy2745/ingress/core/pkg/ingress"
	"github.com/wy2745/ingress/core/pkg/ingress/annotations/ratelimit"
	ing_net "github.com/wy2745/ingress/core/pkg/net"
	"github.com/wy2745/ingress/core/pkg/watch"
)

const (
	slash         = "/"
	defBufferSize = 65535
)

// Template ...
type Template struct {
	tmpl *text_template.Template
	fw   watch.FileWatcher
	s    int
}

//NewTemplate returns a new Template instance or an
//error if the specified template file contains errors
func NewTemplate(file string, onChange func()) (*Template, error) {
	tmpl, err := text_template.New("nginx.tmpl").Funcs(funcMap).ParseFiles(file)
	if err != nil {
		return nil, err
	}
	fw, err := watch.NewFileWatcher(file, onChange)
	if err != nil {
		return nil, err
	}

	return &Template{
		tmpl: tmpl,
		fw:   fw,
		s:    defBufferSize,
	}, nil
}

// Close removes the file watcher
func (t *Template) Close() {
	t.fw.Close()
}

// Write populates a buffer using a template with NGINX configuration
// and the servers and upstreams created by Ingress rules
func (t *Template) Write(conf config.TemplateConfig) ([]byte, error) {
	tmplBuf := bytes.NewBuffer(make([]byte, 0, t.s))
	outCmdBuf := bytes.NewBuffer(make([]byte, 0, t.s))

	defer func() {
		if t.s < tmplBuf.Cap() {
			glog.V(2).Infof("adjusting template buffer size from %v to %v", t.s, tmplBuf.Cap())
			t.s = tmplBuf.Cap()
		}
	}()

	if glog.V(3) {
		b, err := json.Marshal(conf)
		if err != nil {
			glog.Errorf("unexpected error: %v", err)
		}
		glog.Infof("NGINX configuration: %v", string(b))
	}

	err := t.tmpl.Execute(tmplBuf, conf)
	if err != nil {
		return nil, err
	}

	// squeezes multiple adjacent empty lines to be single
	// spaced this is to avoid the use of regular expressions
	cmd := exec.Command("/ingress-controller/clean-nginx-conf.sh")
	cmd.Stdin = tmplBuf
	cmd.Stdout = outCmdBuf
	if err := cmd.Run(); err != nil {
		glog.Warningf("unexpected error cleaning template: %v", err)
		return tmplBuf.Bytes(), nil
	}

	return outCmdBuf.Bytes(), nil
}

var (
	funcMap = text_template.FuncMap{
		"empty": func(input interface{}) bool {
			check, ok := input.(string)
			if ok {
				return len(check) == 0
			}
			return true
		},
		"buildLocation":            buildLocation,
		"buildAuthLocation":        buildAuthLocation,
		"buildAuthResponseHeaders": buildAuthResponseHeaders,
		"buildProxyPass":           buildProxyPass,
		"filterRateLimits":         filterRateLimits,
		"buildRateLimitZones":      buildRateLimitZones,
		"buildRateLimit":           buildRateLimit,
		"buildResolvers":           buildResolvers,
		"buildUpstreamName":        buildUpstreamName,
		"isLocationAllowed":        isLocationAllowed,
		"buildLogFormatUpstream":   buildLogFormatUpstream,
		"buildDenyVariable":        buildDenyVariable,
		"getenv":                   os.Getenv,
		"contains":                 strings.Contains,
		"hasPrefix":                strings.HasPrefix,
		"hasSuffix":                strings.HasSuffix,
		"toUpper":                  strings.ToUpper,
		"toLower":                  strings.ToLower,
		"formatIP":                 formatIP,
		"buildNextUpstream":        buildNextUpstream,
		"getIngressInformation":    getIngressInformation,
		"serverConfig": func(all config.TemplateConfig, server *ingress.Server) interface{} {
			return struct{ First, Second interface{} }{all, server}
		},
		"buildAuthSignURL":            buildAuthSignURL,
		"isValidClientBodyBufferSize": isValidClientBodyBufferSize,
		"buildForwardedFor":           buildForwardedFor,
	}
)

// formatIP will wrap IPv6 addresses in [] and return IPv4 addresses
// without modification. If the input cannot be parsed as an IP address
// it is returned without modification.
func formatIP(input string) string {
	ip := net.ParseIP(input)
	if ip == nil {
		return input
	}
	if v4 := ip.To4(); v4 != nil {
		return input
	}
	return fmt.Sprintf("[%s]", input)
}

// buildResolvers returns the resolvers reading the /etc/resolv.conf file
func buildResolvers(input interface{}) string {
	// NGINX need IPV6 addresses to be surrounded by brackets
	nss, ok := input.([]net.IP)
	if !ok {
		glog.Errorf("expected a '[]net.IP' type but %T was returned", input)
		return ""
	}

	if len(nss) == 0 {
		return ""
	}

	r := []string{"resolver"}
	for _, ns := range nss {
		if ing_net.IsIPV6(ns) {
			r = append(r, fmt.Sprintf("[%v]", ns))
		} else {
			r = append(r, fmt.Sprintf("%v", ns))
		}
	}
	r = append(r, "valid=30s;")

	return strings.Join(r, " ")
}

// buildLocation produces the location string, if the ingress has redirects
// (specified through the ingress.kubernetes.io/rewrite-to annotation)
func buildLocation(input interface{}) string {
	location, ok := input.(*ingress.Location)
	if !ok {
		glog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return slash
	}

	path := location.Path
	if len(location.Rewrite.Target) > 0 && location.Rewrite.Target != path {
		if path == slash {
			return fmt.Sprintf("~* %s", path)
		}
		// baseuri regex will parse basename from the given location
		baseuri := `(?<baseuri>.*)`
		if !strings.HasSuffix(path, slash) {
			// Not treat the slash after "location path" as a part of baseuri
			baseuri = fmt.Sprintf(`\/?%s`, baseuri)
		}
		return fmt.Sprintf(`~* ^%s%s`, path, baseuri)
	}

	return path
}

// TODO: Needs Unit Tests
func buildAuthLocation(input interface{}) string {
	location, ok := input.(*ingress.Location)
	if !ok {
		glog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return ""
	}

	if location.ExternalAuth.URL == "" {
		return ""
	}

	str := base64.URLEncoding.EncodeToString([]byte(location.Path))
	// avoid locations containing the = char
	str = strings.Replace(str, "=", "", -1)
	return fmt.Sprintf("/_external-auth-%v", str)
}

func buildAuthResponseHeaders(input interface{}) []string {
	location, ok := input.(*ingress.Location)
	res := []string{}
	if !ok {
		glog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return res
	}

	if len(location.ExternalAuth.ResponseHeaders) == 0 {
		return res
	}

	for i, h := range location.ExternalAuth.ResponseHeaders {
		hvar := strings.ToLower(h)
		hvar = strings.NewReplacer("-", "_").Replace(hvar)
		res = append(res, fmt.Sprintf("auth_request_set $authHeader%v $upstream_http_%v;", i, hvar))
		res = append(res, fmt.Sprintf("proxy_set_header '%v' $authHeader%v;", h, i))
	}
	return res
}

func buildLogFormatUpstream(input interface{}) string {
	cfg, ok := input.(config.Configuration)
	if !ok {
		glog.Errorf("expected a 'config.Configuration' type but %T was returned", input)
		return ""
	}

	return cfg.BuildLogFormatUpstream()
}

// buildProxyPass produces the proxy pass string, if the ingress has redirects
// (specified through the ingress.kubernetes.io/rewrite-to annotation)
// If the annotation ingress.kubernetes.io/add-base-url:"true" is specified it will
// add a base tag in the head of the response from the service
func buildProxyPass(host string, b interface{}, loc interface{}) string {
	backends, ok := b.([]*ingress.Backend)
	if !ok {
		glog.Errorf("expected an '[]*ingress.Backend' type but %T was returned", b)
		return ""
	}

	location, ok := loc.(*ingress.Location)
	if !ok {
		glog.Errorf("expected a '*ingress.Location' type but %T was returned", loc)
		return ""
	}

	path := location.Path
	proto := "http"

	upstreamName := location.Backend
	for _, backend := range backends {
		if backend.Name == location.Backend {
			if backend.Secure || backend.SSLPassthrough {
				proto = "https"
			}

			if isSticky(host, location, backend.SessionAffinity.CookieSessionAffinity.Locations) {
				upstreamName = fmt.Sprintf("sticky-%v", upstreamName)
			}

			break
		}
	}

	// defProxyPass returns the default proxy_pass, just the name of the upstream
	defProxyPass := fmt.Sprintf("proxy_pass %s://%s;", proto, upstreamName)
	// if the path in the ingress rule is equals to the target: no special rewrite
	if path == location.Rewrite.Target {
		return defProxyPass
	}

	if !strings.HasSuffix(path, slash) {
		path = fmt.Sprintf("%s/", path)
	}

	if len(location.Rewrite.Target) > 0 {
		abu := ""
		if location.Rewrite.AddBaseURL {
			// path has a slash suffix, so that it can be connected with baseuri directly
			bPath := fmt.Sprintf("%s%s", path, "$baseuri")
			if len(location.Rewrite.BaseURLScheme) > 0 {
				abu = fmt.Sprintf(`subs_filter '<head(.*)>' '<head$1><base href="%v://$http_host%v">' r;
	    subs_filter '<HEAD(.*)>' '<HEAD$1><base href="%v://$http_host%v">' r;
	    `, location.Rewrite.BaseURLScheme, bPath, location.Rewrite.BaseURLScheme, bPath)
			} else {
				abu = fmt.Sprintf(`subs_filter '<head(.*)>' '<head$1><base href="$scheme://$http_host%v">' r;
	    subs_filter '<HEAD(.*)>' '<HEAD$1><base href="$scheme://$http_host%v">' r;
	    `, bPath, bPath)
			}
		}

		if location.Rewrite.Target == slash {
			// special case redirect to /
			// ie /something to /
			return fmt.Sprintf(`
	    rewrite %s(.*) /$1 break;
	    rewrite %s / break;
	    proxy_pass %s://%s;
	    %v`, path, location.Path, proto, upstreamName, abu)
		}

		return fmt.Sprintf(`
	    rewrite %s(.*) %s/$1 break;
	    proxy_pass %s://%s;
	    %v`, path, location.Rewrite.Target, proto, upstreamName, abu)
	}

	// default proxy_pass
	return defProxyPass
}

// TODO: Needs Unit Tests
func filterRateLimits(input interface{}) []ratelimit.RateLimit {
	ratelimits := []ratelimit.RateLimit{}
	found := sets.String{}

	servers, ok := input.([]*ingress.Server)
	if !ok {
		glog.Errorf("expected a '[]ratelimit.RateLimit' type but %T was returned", input)
		return ratelimits
	}
	for _, server := range servers {
		for _, loc := range server.Locations {
			if loc.RateLimit.ID != "" && !found.Has(loc.RateLimit.ID) {
				found.Insert(loc.RateLimit.ID)
				ratelimits = append(ratelimits, loc.RateLimit)
			}
		}
	}
	return ratelimits
}

// TODO: Needs Unit Tests
// buildRateLimitZones produces an array of limit_conn_zone in order to allow
// rate limiting of request. Each Ingress rule could have up to three zones, one
// for connection limit by IP address, one for limiting requests per minute, and
// one for limiting requests per second.
func buildRateLimitZones(input interface{}) []string {
	zones := sets.String{}

	servers, ok := input.([]*ingress.Server)
	if !ok {
		glog.Errorf("expected a '[]*ingress.Server' type but %T was returned", input)
		return zones.List()
	}

	for _, server := range servers {
		for _, loc := range server.Locations {
			if loc.RateLimit.Connections.Limit > 0 {
				zone := fmt.Sprintf("limit_conn_zone $limit_%s zone=%v:%vm;",
					loc.RateLimit.ID,
					loc.RateLimit.Connections.Name,
					loc.RateLimit.Connections.SharedSize)
				if !zones.Has(zone) {
					zones.Insert(zone)
				}
			}

			if loc.RateLimit.RPM.Limit > 0 {
				zone := fmt.Sprintf("limit_req_zone $limit_%s zone=%v:%vm rate=%vr/m;",
					loc.RateLimit.ID,
					loc.RateLimit.RPM.Name,
					loc.RateLimit.RPM.SharedSize,
					loc.RateLimit.RPM.Limit)
				if !zones.Has(zone) {
					zones.Insert(zone)
				}
			}

			if loc.RateLimit.RPS.Limit > 0 {
				zone := fmt.Sprintf("limit_req_zone $limit_%s zone=%v:%vm rate=%vr/s;",
					loc.RateLimit.ID,
					loc.RateLimit.RPS.Name,
					loc.RateLimit.RPS.SharedSize,
					loc.RateLimit.RPS.Limit)
				if !zones.Has(zone) {
					zones.Insert(zone)
				}
			}
		}
	}

	return zones.List()
}

// buildRateLimit produces an array of limit_req to be used inside the Path of
// Ingress rules. The order: connections by IP first, then RPS, and RPM last.
func buildRateLimit(input interface{}) []string {
	limits := []string{}

	loc, ok := input.(*ingress.Location)
	if !ok {
		glog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return limits
	}

	if loc.RateLimit.Connections.Limit > 0 {
		limit := fmt.Sprintf("limit_conn %v %v;",
			loc.RateLimit.Connections.Name, loc.RateLimit.Connections.Limit)
		limits = append(limits, limit)
	}

	if loc.RateLimit.RPS.Limit > 0 {
		limit := fmt.Sprintf("limit_req zone=%v burst=%v nodelay;",
			loc.RateLimit.RPS.Name, loc.RateLimit.RPS.Burst)
		limits = append(limits, limit)
	}

	if loc.RateLimit.RPM.Limit > 0 {
		limit := fmt.Sprintf("limit_req zone=%v burst=%v nodelay;",
			loc.RateLimit.RPM.Name, loc.RateLimit.RPM.Burst)
		limits = append(limits, limit)
	}

	if loc.RateLimit.LimitRateAfter > 0 {
		limit := fmt.Sprintf("limit_rate_after %vk;",
			loc.RateLimit.LimitRateAfter)
		limits = append(limits, limit)
	}

	if loc.RateLimit.LimitRate > 0 {
		limit := fmt.Sprintf("limit_rate %vk;",
			loc.RateLimit.LimitRate)
		limits = append(limits, limit)
	}

	return limits
}

func isLocationAllowed(input interface{}) bool {
	loc, ok := input.(*ingress.Location)
	if !ok {
		glog.Errorf("expected an '*ingress.Location' type but %T was returned", input)
		return false
	}

	return loc.Denied == nil
}

var (
	denyPathSlugMap = map[string]string{}
)

// buildDenyVariable returns a nginx variable for a location in a
// server to be used in the whitelist check
// This method uses a unique id generator library to reduce the
// size of the string to be used as a variable in nginx to avoid
// issue with the size of the variable bucket size directive
func buildDenyVariable(a interface{}) string {
	l, ok := a.(string)
	if !ok {
		glog.Errorf("expected a 'string' type but %T was returned", a)
		return ""
	}

	if _, ok := denyPathSlugMap[l]; !ok {
		denyPathSlugMap[l] = buildRandomUUID()
	}

	return fmt.Sprintf("$deny_%v", denyPathSlugMap[l])
}

// TODO: Needs Unit Tests
func buildUpstreamName(host string, b interface{}, loc interface{}) string {

	backends, ok := b.([]*ingress.Backend)
	if !ok {
		glog.Errorf("expected an '[]*ingress.Backend' type but %T was returned", b)
		return ""
	}

	location, ok := loc.(*ingress.Location)
	if !ok {
		glog.Errorf("expected a '*ingress.Location' type but %T was returned", loc)
		return ""
	}

	upstreamName := location.Backend

	for _, backend := range backends {
		if backend.Name == location.Backend {
			if backend.SessionAffinity.AffinityType == "cookie" &&
				isSticky(host, location, backend.SessionAffinity.CookieSessionAffinity.Locations) {
				upstreamName = fmt.Sprintf("sticky-%v", upstreamName)
			}

			break
		}
	}

	return upstreamName
}

// TODO: Needs Unit Tests
func isSticky(host string, loc *ingress.Location, stickyLocations map[string][]string) bool {
	if _, ok := stickyLocations[host]; ok {
		for _, sl := range stickyLocations[host] {
			if sl == loc.Path {
				return true
			}
		}
	}

	return false
}

func buildNextUpstream(input interface{}) string {
	nextUpstream, ok := input.(string)
	if !ok {
		glog.Errorf("expected a 'string' type but %T was returned", input)
		return ""
	}

	parts := strings.Split(nextUpstream, " ")

	nextUpstreamCodes := make([]string, 0, len(parts))
	for _, v := range parts {
		if v != "" && v != "non_idempotent" {
			nextUpstreamCodes = append(nextUpstreamCodes, v)
		}
	}

	return strings.Join(nextUpstreamCodes, " ")
}

func buildAuthSignURL(input interface{}) string {
	s, ok := input.(string)
	if !ok {
		glog.Errorf("expected an 'string' type but %T was returned", input)
		return ""
	}

	u, _ := url.Parse(s)
	q := u.Query()
	if len(q) == 0 {
		return fmt.Sprintf("%v?rd=$request_uri", s)
	}

	return fmt.Sprintf("%v&rd=$request_uri", s)
}

// buildRandomUUID return a random string to be used in the template
func buildRandomUUID() string {
	s := uuid.New()
	return strings.Replace(s, "-", "", -1)
}

func isValidClientBodyBufferSize(input interface{}) bool {
	s, ok := input.(string)
	if !ok {
		glog.Errorf("expected an 'string' type but %T was returned", input)
		return false
	}

	if s == "" {
		return false
	}

	_, err := strconv.Atoi(s)
	if err != nil {
		sLowercase := strings.ToLower(s)

		kCheck := strings.TrimSuffix(sLowercase, "k")
		_, err := strconv.Atoi(kCheck)
		if err == nil {
			return true
		}

		mCheck := strings.TrimSuffix(sLowercase, "m")
		_, err = strconv.Atoi(mCheck)
		if err == nil {
			return true
		}

		glog.Errorf("client-body-buffer-size '%v' was provided in an incorrect format, hence it will not be set.", s)
		return false
	}

	return true
}

type ingressInformation struct {
	Namespace   string
	Rule        string
	Service     string
	Annotations map[string]string
}

func getIngressInformation(i, p interface{}) *ingressInformation {
	ing, ok := i.(*extensions.Ingress)
	if !ok {
		glog.Errorf("expected an '*extensions.Ingress' type but %T was returned", i)
		return &ingressInformation{}
	}

	path, ok := p.(string)
	if !ok {
		glog.Errorf("expected a 'string' type but %T was returned", p)
		return &ingressInformation{}
	}

	if ing == nil {
		return &ingressInformation{}
	}

	info := &ingressInformation{
		Namespace:   ing.GetNamespace(),
		Rule:        ing.GetName(),
		Annotations: ing.Annotations,
	}

	if ing.Spec.Backend != nil {
		info.Service = ing.Spec.Backend.ServiceName
	}

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}

		for _, rPath := range rule.HTTP.Paths {
			if path == rPath.Path {
				info.Service = rPath.Backend.ServiceName
				return info
			}
		}
	}

	return info
}

func buildForwardedFor(input interface{}) string {
	s, ok := input.(string)
	if !ok {
		glog.Errorf("expected a 'string' type but %T was returned", input)
		return ""
	}

	ffh := strings.Replace(s, "-", "_", -1)
	ffh = strings.ToLower(ffh)
	return fmt.Sprintf("$http_%v", ffh)
}
