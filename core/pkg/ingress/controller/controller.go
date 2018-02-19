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
	"math/rand"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"

	cadvisor "github.com/google/cadvisor/info/v1"
	kube_api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"

	heapsterManager "github.com/wy2745/ingress/core/pkg"
	"github.com/wy2745/ingress/core/pkg/ingress"
	"github.com/wy2745/ingress/core/pkg/ingress/annotations/class"
	"github.com/wy2745/ingress/core/pkg/ingress/annotations/healthcheck"
	"github.com/wy2745/ingress/core/pkg/ingress/annotations/parser"
	"github.com/wy2745/ingress/core/pkg/ingress/annotations/proxy"
	"github.com/wy2745/ingress/core/pkg/ingress/defaults"
	"github.com/wy2745/ingress/core/pkg/ingress/resolver"
	"github.com/wy2745/ingress/core/pkg/ingress/status"
	"github.com/wy2745/ingress/core/pkg/ingress/store"
	"github.com/wy2745/ingress/core/pkg/k8s"
	"github.com/wy2745/ingress/core/pkg/net/ssl"
	local_strings "github.com/wy2745/ingress/core/pkg/strings"
	"github.com/wy2745/ingress/core/pkg/task"
	"k8s.io/heapster/metrics/core"
	"k8s.io/heapster/metrics/sources"
	"k8s.io/heapster/metrics/sources/kubelet"
	//kube_api "k8s.io/client-go/pkg/api/v1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/heapster/metrics/processors"
)

const (
	defUpstreamName = "upstream-default-backend"
	defServerName   = "_"
	rootLocation    = "/"

	fakeCertificate    = "default-fake-certificate"
	infraContainerName = "POD"
	// TODO: following constants are copied from k8s, change to use them directly
	kubernetesPodNameLabel      = "io.kubernetes.pod.name"
	kubernetesPodNamespaceLabel = "io.kubernetes.pod.namespace"
	kubernetesPodUID            = "io.kubernetes.pod.uid"
	kubernetesContainerLabel    = "io.kubernetes.container.name"
	ResyncGap                   = 10 * time.Second
	testapp                     = "app-test"
)

var (
	// list of ports that cannot be used by TCP or UDP services
	reservedPorts = []string{"80", "443", "8181", "18080"}

	fakeCertificatePath = ""
	fakeCertificateSHA  = ""

	cloner = conversion.NewCloner()
	// The Kubelet request latencies in microseconds.
	kubeletRequestLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace: "heapster",
			Subsystem: "kubelet",
			Name:      "request_duration_microseconds",
			Help:      "The Kubelet request latencies in microseconds.",
		},
		[]string{"node"},
	)
)

// GenericController holds the boilerplate code required to build an Ingress controlller.
type GenericController struct {
	cfg *Configuration

	ingController  cache.Controller
	endpController cache.Controller
	svcController  cache.Controller
	nodeController cache.Controller
	secrController cache.Controller
	mapController  cache.Controller

	listers *ingress.StoreLister

	annotations annotationExtractor

	recorder record.EventRecorder

	syncQueue *task.Queue

	syncStatus status.Sync

	// local store of SSL certificates
	// (only certificates used in ingress)
	sslCertTracker *sslCertTracker

	syncRateLimiter flowcontrol.RateLimiter

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock *sync.Mutex

	stopCh chan struct{}

	// runningConfig contains the running configuration in the Backend
	runningConfig *ingress.Configuration

	forceReload int32

	initialSyncDone int32

	//为了方便获取负载加入的变量
	heapsterManager heapsterManager.Manager

	//sourceManager core.MetricsSource

	//dataProcessors []core.DataProcessor
}

// Configuration contains all the settings required by an Ingress controller
type Configuration struct {
	Client        clientset.Interface
	KubeletClient *kubelet.KubeletClient

	ResyncPeriod   time.Duration
	DefaultService string
	IngressClass   string
	Namespace      string
	ConfigMapName  string

	ForceNamespaceIsolation bool
	DisableNodeList         bool

	// optional
	TCPConfigMapName string
	// optional
	UDPConfigMapName      string
	DefaultSSLCertificate string
	DefaultHealthzURL     string
	DefaultIngressClass   string
	// optional
	PublishService string
	// Backend is the particular implementation to be used.
	// (for instance NGINX)
	Backend ingress.Controller

	UpdateStatus           bool
	ElectionID             string
	UpdateStatusOnShutdown bool
	SortBackends           bool
}

type kubeletProvider struct {
	nodeLister    store.NodeLister
	reflector     *cache.Reflector
	kubeletClient *kubelet.KubeletClient
}

// Kubelet-provided metrics for pod and system container.
type kubeletMetricsSource struct {
	host          kubelet.Host
	kubeletClient *kubelet.KubeletClient
	nodename      string
	hostname      string
	hostId        string
}

func (this *kubeletMetricsSource) Name() string {
	return this.String()
}

func (this *kubeletMetricsSource) String() string {
	return fmt.Sprintf("kubelet:%s:%d", this.host.IP, this.host.Port)
}

func (this *kubeletMetricsSource) handleSystemContainer(c *cadvisor.ContainerInfo, cMetrics *core.MetricSet) string {
	glog.V(8).Infof("Found system container %v with labels: %+v", c.Name, c.Spec.Labels)
	cName := c.Name
	if strings.HasPrefix(cName, "/") {
		cName = cName[1:]
	}
	cMetrics.Labels[core.LabelMetricSetType.Key] = core.MetricSetTypeSystemContainer
	cMetrics.Labels[core.LabelContainerName.Key] = cName
	return core.NodeContainerKey(this.nodename, cName)
}

//这个函数会提取pod或者container的数据-----需要考虑到的是，同一个pod里面的多个container之间资源的互相影响
func (this *kubeletMetricsSource) handleKubernetesContainer(cName, ns, podName string, c *cadvisor.ContainerInfo, cMetrics *core.MetricSet) (string, bool) {
	var metricSetKey string
	if cName == infraContainerName {
		if strings.Contains(podName, testapp) {
			metricSetKey = core.PodKey(ns, podName)
			cMetrics.Labels[core.LabelMetricSetType.Key] = core.MetricSetTypePod
		} else {
			return "", false
		}

	} else {
		if strings.Contains(podName, testapp) {
			metricSetKey = core.PodContainerKey(ns, podName, cName)
			cMetrics.Labels[core.LabelMetricSetType.Key] = core.MetricSetTypePodContainer
			cMetrics.Labels[core.LabelContainerName.Key] = cName
			cMetrics.Labels[core.LabelContainerBaseImage.Key] = c.Spec.Image
		} else {
			return "", false
		}
	}
	cMetrics.Labels[core.LabelPodId.Key] = c.Spec.Labels[kubernetesPodUID]
	cMetrics.Labels[core.LabelPodName.Key] = podName
	cMetrics.Labels[core.LabelNamespaceName.Key] = ns
	// Needed for backward compatibility
	cMetrics.Labels[core.LabelPodNamespace.Key] = ns
	return metricSetKey, true
}
func isNode(c *cadvisor.ContainerInfo) bool {
	return c.Name == "/"
}

//负责处理获得的负载数据的，筛选数据可以从这里着手
//只要不是pod或者container就可以不返回数据
func (this *kubeletMetricsSource) decodeMetrics(c *cadvisor.ContainerInfo) (string, *core.MetricSet) {
	if len(c.Stats) == 0 {
		return "", nil
	}

	var metricSetKey string
	cMetrics := &core.MetricSet{
		CreateTime:   c.Spec.CreationTime,
		ScrapeTime:   c.Stats[0].Timestamp,
		MetricValues: map[string]core.MetricValue{},
		Labels: map[string]string{
			core.LabelNodename.Key: this.nodename,
			core.LabelHostname.Key: this.hostname,
			core.LabelHostID.Key:   this.hostId,
		},
		LabeledMetrics: []core.LabeledMetric{},
	}

	if isNode(c) {
		metricSetKey = core.NodeKey(this.nodename)
		cMetrics.Labels[core.LabelMetricSetType.Key] = core.MetricSetTypeNode
	} else {
		cName := c.Spec.Labels[kubernetesContainerLabel]
		ns := c.Spec.Labels[kubernetesPodNamespaceLabel]
		podName := c.Spec.Labels[kubernetesPodNameLabel]

		// Support for kubernetes 1.0.*
		if ns == "" && strings.Contains(podName, "/") {
			tokens := strings.SplitN(podName, "/", 2)
			if len(tokens) == 2 {
				ns = tokens[0]
				podName = tokens[1]
			}
		}
		if cName == "" {
			// Better this than nothing. This is a temporary hack for new heapster to work
			// with Kubernetes 1.0.*.
			// TODO: fix this with POD list.
			// Parsing name like:
			// k8s_kube-ui.7f9b83f6_kube-ui-v1-bxj1w_kube-system_9abfb0bd-811f-11e5-b548-42010af00002_e6841e8d
			pos := strings.Index(c.Name, ".")
			if pos >= 0 {
				// remove first 4 chars.
				cName = c.Name[len("k8s_"):pos]
			}
		}

		// No Kubernetes metadata so treat this as a system container.
		//这里有个system container和kubernetes container的区别
		//暂时考虑不加入系统的container
		if cName == "" || ns == "" || podName == "" {
			//metricSetKey = this.handleSystemContainer(c, cMetrics)
			//如果是系统的container，直接返回
			return "", nil
		} else {
			var ok bool
			metricSetKey, ok = this.handleKubernetesContainer(cName, ns, podName, c, cMetrics)
			if ok == false {
				return "", nil
			}
		}
	}

	//这里是能用到的基本的metric
	for _, metric := range core.StandardMetrics {
		if metric.HasValue != nil && metric.HasValue(&c.Spec) {
			cMetrics.MetricValues[metric.Name] = metric.GetValue(&c.Spec, c.Stats[0])
		}
	}

	//这是基本metric
	for _, metric := range core.LabeledMetrics {
		if metric.HasLabeledMetric != nil && metric.HasLabeledMetric(&c.Spec) {
			labeledMetrics := metric.GetLabeledMetric(&c.Spec, c.Stats[0])
			cMetrics.LabeledMetrics = append(cMetrics.LabeledMetrics, labeledMetrics...)
		}
	}

	if c.Spec.HasCustomMetrics {
	metricloop:
		for _, spec := range c.Spec.CustomMetrics {
			if cmValue, ok := c.Stats[0].CustomMetrics[spec.Name]; ok && cmValue != nil && len(cmValue) >= 1 {
				newest := cmValue[0]
				for _, metricVal := range cmValue {
					if newest.Timestamp.Before(metricVal.Timestamp) {
						newest = metricVal
					}
				}
				mv := core.MetricValue{}
				switch spec.Type {
				case cadvisor.MetricGauge:
					mv.MetricType = core.MetricGauge
				case cadvisor.MetricCumulative:
					mv.MetricType = core.MetricCumulative
				default:
					glog.V(4).Infof("Skipping %s: unknown custom metric type: %v", spec.Name, spec.Type)
					continue metricloop
				}

				switch spec.Format {
				case cadvisor.IntType:
					mv.ValueType = core.ValueInt64
					mv.IntValue = newest.IntValue
				case cadvisor.FloatType:
					mv.ValueType = core.ValueFloat
					mv.FloatValue = float32(newest.FloatValue)
				default:
					glog.V(4).Infof("Skipping %s: unknown custom metric format", spec.Name, spec.Format)
					continue metricloop
				}

				cMetrics.MetricValues[core.CustomMetricPrefix+spec.Name] = mv
			}
		}
	}

	return metricSetKey, cMetrics
}

//在这里对这个函数进行改造，改造思路，对于每个container，保存一个队列，存在近30s中的六组以5s为间隔的负载数据，，以及这六组数据的负载总和，一旦有新的数据加入，从队列中剔除最旧的数据加入新数据，用总和减去旧数据，加上新数据
func (this *kubeletMetricsSource) ScrapeMetrics(start, end time.Time) *core.DataBatch {
	//这里是对container负载的获取
	containers, err := this.scrapeKubelet(this.kubeletClient, this.host, start, end)
	if err != nil {
		glog.Errorf("error while getting containers from Kubelet: %v", err)
	}
	glog.V(2).Infof("successfully obtained stats for %v containers", len(containers))

	result := &core.DataBatch{
		Timestamp:  end,
		MetricSets: map[string]*core.MetricSet{},
	}
	keys := make(map[string]bool)

	for _, c := range containers {
		name, metrics := this.decodeMetrics(&c)
		if name == "" || metrics == nil {
			continue
		}
		result.MetricSets[name] = metrics
		keys[name] = true
	}
	return result
}

func (this *kubeletMetricsSource) scrapeKubelet(client *kubelet.KubeletClient, host kubelet.Host, start, end time.Time) ([]cadvisor.ContainerInfo, error) {
	startTime := time.Now()
	defer kubeletRequestLatency.WithLabelValues(this.hostname).Observe(float64(time.Since(startTime)))
	return client.GetAllRawContainers(host, start, end)
}

func NewKubeletMetricsSource(host kubelet.Host, client *kubelet.KubeletClient, nodeName string, hostName string, hostId string) core.MetricsSource {
	return &kubeletMetricsSource{
		host:          host,
		kubeletClient: client,
		nodename:      nodeName,
		hostname:      hostName,
		hostId:        hostId,
	}
}
func getNodeHostnameAndIP(node *kube_api.Node) (string, string, error) {
	for _, c := range node.Status.Conditions {
		if c.Type == kube_api.NodeReady && c.Status != kube_api.ConditionTrue {
			return "", "", fmt.Errorf("Node %v is not ready", node.Name)
		}
	}
	hostname, ip := node.Name, ""
	for _, addr := range node.Status.Addresses {
		if addr.Type == kube_api.NodeHostName && addr.Address != "" {
			hostname = addr.Address
		}
		if addr.Type == kube_api.NodeInternalIP && addr.Address != "" {
			if net.ParseIP(addr.Address).To4() != nil {
				ip = addr.Address
			}
		}
		if addr.Type == kube_api.NodeLegacyHostIP && addr.Address != "" && ip == "" {
			ip = addr.Address
		}
	}
	if ip != "" {
		return hostname, ip, nil
	}
	return "", "", fmt.Errorf("Node %v has no valid hostname and/or IP address: %v %v", node.Name, hostname, ip)
}

//这个函数会对每一个node都生成一个KubeletMetricsSource
func (this *kubeletProvider) GetMetricsSources() []core.MetricsSource {
	sources := []core.MetricsSource{}
	nodes, err := this.nodeLister.List(labels.Everything())
	if err != nil {
		glog.Errorf("error while listing nodes: %v", err)
		return sources
	}
	if len(nodes) == 0 {
		glog.Error("No nodes received from APIserver.")
		return sources
	}

	nodeNames := make(map[string]bool)
	for _, node := range nodes {
		nodeNames[node.Name] = true
		hostname, ip, err := getNodeHostnameAndIP(node)
		if err != nil {
			glog.Errorf("%v", err)
			continue
		}
		sources = append(sources, NewKubeletMetricsSource(
			kubelet.Host{IP: ip, Port: this.kubeletClient.GetPort()},
			this.kubeletClient,
			node.Name,
			hostname,
			node.Spec.ExternalID,
		))
	}
	return sources
}

func (ic *GenericController) NewKubeletProvider() (core.MetricsSourceProvider, error) {

	return &kubeletProvider{
		nodeLister:    ic.listers.Node,
		reflector:     ic.listers.NodeReflector,
		kubeletClient: ic.cfg.KubeletClient,
	}, nil
}

// newIngressController creates an Ingress controller
func newIngressController(config *Configuration) *GenericController {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{
		Interface: config.Client.CoreV1().Events(config.Namespace),
	})

	ic := GenericController{
		cfg:             config,
		stopLock:        &sync.Mutex{},
		stopCh:          make(chan struct{}),
		syncRateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.3, 1),
		recorder: eventBroadcaster.NewRecorder(scheme.Scheme, kube_api.EventSource{
			Component: "ingress-controller",
		}),
		sslCertTracker: newSSLCertTracker(),
		listers:        &ingress.StoreLister{},
	}

	ic.syncQueue = task.NewTaskQueue(ic.syncIngress)

	//对event，ingress等建立了lister
	ic.createListers(config.DisableNodeList)
	//在这里需要设定一个KubeletProvider以供负载的采集
	kubeletProvider, err := ic.NewKubeletProvider()
	if err != nil {
		fmt.Println(err)
	}
	//需要在这里建立新的sourceManager和dataProcessors
	sourceManager, err := sources.NewSourceManager(kubeletProvider, sources.DefaultMetricsScrapeTimeout)
	dataProcessors := ic.createDataProcessorsOrDie(ic.listers.Pod)
	ic.heapsterManager, err = heapsterManager.NewManager(sourceManager, dataProcessors,
		ResyncGap, heapsterManager.DefaultScrapeOffset, heapsterManager.DefaultMaxParallelism)
	if err != nil {
		glog.Fatalf("Failed to create heapster manager: %v", err)
	}

	if err != nil {
		fmt.Println(err)
	}

	if config.UpdateStatus {
		ic.syncStatus = status.NewStatusSyncer(status.Config{
			Client:                 config.Client,
			PublishService:         ic.cfg.PublishService,
			IngressLister:          ic.listers.Ingress,
			ElectionID:             config.ElectionID,
			IngressClass:           config.IngressClass,
			DefaultIngressClass:    config.DefaultIngressClass,
			UpdateStatusOnShutdown: config.UpdateStatusOnShutdown,
			CustomIngressStatus:    ic.cfg.Backend.UpdateIngressStatus,
		})
	} else {
		glog.Warning("Update of ingress status is disabled (flag --update-status=false was specified)")
	}
	ic.annotations = newAnnotationExtractor(ic)

	ic.cfg.Backend.SetListers(ic.listers)

	cloner.RegisterDeepCopyFunc(ingress.GetGeneratedDeepCopyFuncs)

	return &ic
}

func (ic *GenericController) createDataProcessorsOrDie(podLister v1.PodLister) []core.DataProcessor {
	dataProcessors := []core.DataProcessor{
		// Convert cumulative to rate
		processors.NewRateCalculator(core.RateMetricsMapping),
	}

	podBasedEnricher, err := processors.NewPodBasedEnricher(podLister)
	if err != nil {
		glog.Fatalf("Failed to create PodBasedEnricher: %v", err)
	}
	dataProcessors = append(dataProcessors, podBasedEnricher)

	namespaceBasedEnricher, err := processors.NewNamespaceBasedEnricher(ic.cfg.Client)
	if err != nil {
		glog.Fatalf("Failed to create NamespaceBasedEnricher: %v", err)
	}
	dataProcessors = append(dataProcessors, namespaceBasedEnricher)

	// aggregators
	metricsToAggregate := []string{
		core.MetricCpuUsageRate.Name,
		core.MetricMemoryUsage.Name,
		core.MetricCpuRequest.Name,
		core.MetricCpuLimit.Name,
		core.MetricMemoryRequest.Name,
		core.MetricMemoryLimit.Name,
	}

	metricsToAggregateForNode := []string{
		core.MetricCpuRequest.Name,
		core.MetricCpuLimit.Name,
		core.MetricMemoryRequest.Name,
		core.MetricMemoryLimit.Name,
	}

	dataProcessors = append(dataProcessors,
		processors.NewPodAggregator(),
		&processors.NamespaceAggregator{
			MetricsToAggregate: metricsToAggregate,
		},
		&processors.NodeAggregator{
			MetricsToAggregate: metricsToAggregateForNode,
		},
		&processors.ClusterAggregator{
			MetricsToAggregate: metricsToAggregate,
		})

	nodeAutoscalingEnricher, err := processors.NewNodeAutoscalingEnricher(ic.listers.Node, ic.listers.NodeReflector)
	if err != nil {
		glog.Fatalf("Failed to create NodeAutoscalingEnricher: %v", err)
	}
	dataProcessors = append(dataProcessors, nodeAutoscalingEnricher)
	return dataProcessors
}

// Info returns information about the backend
func (ic GenericController) Info() *ingress.BackendInfo {
	return ic.cfg.Backend.Info()
}

// IngressClass returns information about the backend
func (ic GenericController) IngressClass() string {
	return ic.cfg.IngressClass
}

// GetDefaultBackend returns the default backend
func (ic GenericController) GetDefaultBackend() defaults.Backend {
	return ic.cfg.Backend.BackendDefaults()
}

// GetPublishService returns the configured service used to set ingress status
func (ic GenericController) GetPublishService() *kube_api.Service {
	s, err := ic.listers.Service.GetByName(ic.cfg.PublishService)
	if err != nil {
		return nil
	}

	return s
}

// GetRecorder returns the event recorder
func (ic GenericController) GetRecorder() record.EventRecorder {
	return ic.recorder
}

// GetSecret searches for a secret in the local secrets Store
func (ic GenericController) GetSecret(name string) (*kube_api.Secret, error) {
	return ic.listers.Secret.GetByName(name)
}

// GetService searches for a service in the local secrets Store
func (ic GenericController) GetService(name string) (*kube_api.Service, error) {
	return ic.listers.Service.GetByName(name)
}

// sync collects all the pieces required to assemble the configuration file and
// then sends the content to the backend (OnUpdate) receiving the populated
// template as response reloading the backend if is required.
func (ic *GenericController) syncIngress(key interface{}) error {
	ic.syncRateLimiter.Accept()

	if ic.syncQueue.IsShuttingDown() {
		return nil
	}

	if name, ok := key.(string); ok {
		if obj, exists, _ := ic.listers.Ingress.GetByKey(name); exists {
			ing := obj.(*extensions.Ingress)
			ic.readSecrets(ing)
		}
	}

	// Sort ingress rules using the ResourceVersion field
	ings := ic.listers.Ingress.List()
	sort.SliceStable(ings, func(i, j int) bool {
		ir := ings[i].(*ingress.SSLCert).ResourceVersion
		jr := ings[j].(*ingress.SSLCert).ResourceVersion
		return ir < jr
	})

	// filter ingress rules
	var ingresses []*extensions.Ingress
	for _, ingIf := range ings {
		ing := ingIf.(*extensions.Ingress)
		if !class.IsValid(ing, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
			continue
		}

		ingresses = append(ingresses, ing)
	}

	//这一句是一个关键，会更新upstream和server
	upstreams, servers := ic.getBackendServers(ingresses)
	var passUpstreams []*ingress.SSLPassthroughBackend

	for _, server := range servers {
		if !server.SSLPassthrough {
			continue
		}

		for _, loc := range server.Locations {
			if loc.Path != rootLocation {
				glog.Warningf("ignoring path %v of ssl passthrough host %v", loc.Path, server.Hostname)
				continue
			}
			passUpstreams = append(passUpstreams, &ingress.SSLPassthroughBackend{
				Backend:  loc.Backend,
				Hostname: server.Hostname,
				Service:  loc.Service,
				Port:     loc.Port,
			})
			break
		}
	}

	pcfg := ingress.Configuration{
		Backends: upstreams,
		Servers:  servers,
		//这里会获取service相应的endpoint更新到配置文件里面去
		TCPEndpoints:        ic.getStreamServices(ic.cfg.TCPConfigMapName, kube_api.ProtocolTCP),
		UDPEndpoints:        ic.getStreamServices(ic.cfg.UDPConfigMapName, kube_api.ProtocolUDP),
		PassthroughBackends: passUpstreams,
	}

	if !ic.isForceReload() && ic.runningConfig != nil && ic.runningConfig.Equal(&pcfg) {
		glog.V(3).Infof("skipping backend reload (no changes detected)")
		return nil
	}

	glog.Infof("backend reload required")
	//这个OnUpdate函数用于将更新的配置文件更新应用到实际上去
	err := ic.cfg.Backend.OnUpdate(pcfg)
	if err != nil {
		incReloadErrorCount()
		glog.Errorf("unexpected failure restarting the backend: \n%v", err)
		return err
	}

	glog.Infof("ingress backend successfully reloaded...")
	incReloadCount()
	setSSLExpireTime(servers)

	ic.runningConfig = &pcfg
	ic.setForceReload(false)

	return nil
}

func (ic *GenericController) getStreamServices(configmapName string, proto kube_api.Protocol) []ingress.L4Service {
	glog.V(3).Infof("obtaining information about stream services of type %v located in configmap %v", proto, configmapName)
	if configmapName == "" {
		// no configmap configured
		return []ingress.L4Service{}
	}

	_, _, err := k8s.ParseNameNS(configmapName)
	if err != nil {
		glog.Errorf("unexpected error reading configmap %v: %v", configmapName, err)
		return []ingress.L4Service{}
	}

	configmap, err := ic.listers.ConfigMap.GetByName(configmapName)
	if err != nil {
		glog.Errorf("unexpected error reading configmap %v: %v", configmapName, err)
		return []ingress.L4Service{}
	}

	var svcs []ingress.L4Service
	// k -> port to expose
	// v -> <namespace>/<service name>:<port from service to be used>
	for k, v := range configmap.Data {
		externalPort, err := strconv.Atoi(k)
		if err != nil {
			glog.Warningf("%v is not valid as a TCP/UDP port", k)
			continue
		}

		// this ports used by the backend
		if local_strings.StringInSlice(k, reservedPorts) {
			glog.Warningf("port %v cannot be used for TCP or UDP services. It is reserved for the Ingress controller", k)
			continue
		}

		nsSvcPort := strings.Split(v, ":")
		if len(nsSvcPort) < 2 {
			glog.Warningf("invalid format (namespace/name:port:[PROXY]) '%v'", k)
			continue
		}

		nsName := nsSvcPort[0]
		svcPort := nsSvcPort[1]
		useProxyProtocol := false

		// Proxy protocol is possible if the service is TCP
		if len(nsSvcPort) == 3 && proto == kube_api.ProtocolTCP {
			if strings.ToUpper(nsSvcPort[2]) == "PROXY" {
				useProxyProtocol = true
			}
		}

		svcNs, svcName, err := k8s.ParseNameNS(nsName)
		if err != nil {
			glog.Warningf("%v", err)
			continue
		}

		svcObj, svcExists, err := ic.listers.Service.GetByKey(nsName)
		if err != nil {
			glog.Warningf("error getting service %v: %v", nsName, err)
			continue
		}

		if !svcExists {
			glog.Warningf("service %v was not found", nsName)
			continue
		}

		svc := svcObj.(*kube_api.Service)

		var endps []ingress.Endpoint
		targetPort, err := strconv.Atoi(svcPort)
		if err != nil {
			glog.V(3).Infof("searching service %v endpoints using the name '%v'", svcNs, svcName, svcPort)
			for _, sp := range svc.Spec.Ports {
				if sp.Name == svcPort {
					if sp.Protocol == proto {
						endps = ic.getEndpoints(svc, &sp, proto, &healthcheck.Upstream{})
						break
					}
				}
			}
		} else {
			// we need to use the TargetPort (where the endpoints are running)
			glog.V(3).Infof("searching service %v/%v endpoints using the target port '%v'", svcNs, svcName, targetPort)
			for _, sp := range svc.Spec.Ports {
				if sp.Port == int32(targetPort) {
					if sp.Protocol == proto {
						endps = ic.getEndpoints(svc, &sp, proto, &healthcheck.Upstream{})
						break
					}
				}
			}
		}

		// stream services cannot contain empty upstreams and there is no
		// default backend equivalent
		if len(endps) == 0 {
			glog.Warningf("service %v/%v does not have any active endpoints for port %v and protocol %v", svcNs, svcName, svcPort, proto)
			continue
		}

		svcs = append(svcs, ingress.L4Service{
			Port: externalPort,
			Backend: ingress.L4Backend{
				Name:             svcName,
				Namespace:        svcNs,
				Port:             intstr.FromString(svcPort),
				Protocol:         proto,
				UseProxyProtocol: useProxyProtocol,
			},
			Endpoints: endps,
		})
	}

	return svcs
}

// getDefaultUpstream returns an upstream associated with the
// default backend service. In case of error retrieving information
// configure the upstream to return http code 503.
func (ic *GenericController) getDefaultUpstream() *ingress.Backend {
	upstream := &ingress.Backend{
		Name: defUpstreamName,
	}
	svcKey := ic.cfg.DefaultService
	svcObj, svcExists, err := ic.listers.Service.GetByKey(svcKey)
	if err != nil {
		glog.Warningf("unexpected error searching the default backend %v: %v", ic.cfg.DefaultService, err)
		upstream.Endpoints = append(upstream.Endpoints, ic.cfg.Backend.DefaultEndpoint())
		return upstream
	}

	if !svcExists {
		glog.Warningf("service %v does not exist", svcKey)
		upstream.Endpoints = append(upstream.Endpoints, ic.cfg.Backend.DefaultEndpoint())
		return upstream
	}

	svc := svcObj.(*kube_api.Service)
	endps := ic.getEndpoints(svc, &svc.Spec.Ports[0], kube_api.ProtocolTCP, &healthcheck.Upstream{})
	if len(endps) == 0 {
		glog.Warningf("service %v does not have any active endpoints", svcKey)
		endps = []ingress.Endpoint{ic.cfg.Backend.DefaultEndpoint()}
	}

	upstream.Service = svc
	upstream.Endpoints = append(upstream.Endpoints, endps...)
	return upstream
}

// getBackendServers returns a list of Upstream and Server to be used by the backend
// An upstream can be used in multiple servers if the namespace, service name and port are the same
func (ic *GenericController) getBackendServers(ingresses []*extensions.Ingress) ([]*ingress.Backend, []*ingress.Server) {
	du := ic.getDefaultUpstream()
	upstreams := ic.createUpstreams(ingresses, du)
	servers := ic.createServers(ingresses, upstreams, du)

	for _, ing := range ingresses {
		affinity := ic.annotations.SessionAffinity(ing)
		anns := ic.annotations.Extract(ing)

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}
			server := servers[host]
			if server == nil {
				server = servers[defServerName]
			}

			if rule.HTTP == nil &&
				host != defServerName {
				glog.V(3).Infof("ingress rule %v/%v does not contain HTTP rules, using default backend", ing.Namespace, ing.Name)
				continue
			}

			if server.CertificateAuth.CAFileName == "" {
				ca := ic.annotations.CertificateAuth(ing)
				if ca != nil {
					server.CertificateAuth = *ca
				}
			} else {
				glog.V(3).Infof("server %v already contains a muthual autentication configuration - ingress rule %v/%v", server.Hostname, ing.Namespace, ing.Name)
			}

			for _, path := range rule.HTTP.Paths {
				upsName := fmt.Sprintf("%v-%v-%v",
					ing.GetNamespace(),
					path.Backend.ServiceName,
					path.Backend.ServicePort.String())

				ups := upstreams[upsName]

				// if there's no path defined we assume /
				nginxPath := rootLocation
				if path.Path != "" {
					nginxPath = path.Path
				}

				addLoc := true
				for _, loc := range server.Locations {
					if loc.Path == nginxPath {
						addLoc = false

						if !loc.IsDefBackend {
							glog.V(3).Infof("avoiding replacement of ingress rule %v/%v location %v upstream %v (%v)", ing.Namespace, ing.Name, loc.Path, ups.Name, loc.Backend)
							break
						}

						glog.V(3).Infof("replacing ingress rule %v/%v location %v upstream %v (%v)", ing.Namespace, ing.Name, loc.Path, ups.Name, loc.Backend)
						loc.Backend = ups.Name
						loc.IsDefBackend = false
						loc.Backend = ups.Name
						loc.Port = ups.Port
						loc.Service = ups.Service
						loc.Ingress = ing
						mergeLocationAnnotations(loc, anns)
						if loc.Redirect.FromToWWW {
							server.RedirectFromToWWW = true
						}
						break
					}
				}
				// is a new location
				if addLoc {
					glog.V(3).Infof("adding location %v in ingress rule %v/%v upstream %v", nginxPath, ing.Namespace, ing.Name, ups.Name)
					loc := &ingress.Location{
						Path:         nginxPath,
						Backend:      ups.Name,
						IsDefBackend: false,
						Service:      ups.Service,
						Port:         ups.Port,
						Ingress:      ing,
					}
					mergeLocationAnnotations(loc, anns)
					if loc.Redirect.FromToWWW {
						server.RedirectFromToWWW = true
					}
					server.Locations = append(server.Locations, loc)
				}

				if ups.SessionAffinity.AffinityType == "" {
					ups.SessionAffinity.AffinityType = affinity.AffinityType
				}

				if affinity.AffinityType == "cookie" {
					ups.SessionAffinity.CookieSessionAffinity.Name = affinity.CookieConfig.Name
					ups.SessionAffinity.CookieSessionAffinity.Hash = affinity.CookieConfig.Hash

					locs := ups.SessionAffinity.CookieSessionAffinity.Locations
					if _, ok := locs[host]; !ok {
						locs[host] = []string{}
					}

					locs[host] = append(locs[host], path.Path)
				}
			}
		}
	}

	aUpstreams := make([]*ingress.Backend, 0, len(upstreams))

	for _, upstream := range upstreams {
		isHTTPSfrom := []*ingress.Server{}
		for _, server := range servers {
			for _, location := range server.Locations {
				if upstream.Name == location.Backend {
					if len(upstream.Endpoints) == 0 {
						glog.V(3).Infof("upstream %v does not have any active endpoints. Using default backend", upstream.Name)
						location.Backend = "upstream-default-backend"

						// check if the location contains endpoints and a custom default backend
						if location.DefaultBackend != nil {
							sp := location.DefaultBackend.Spec.Ports[0]
							//获取endpoint的部分，修改权重需要从这里入手
							endps := ic.getEndpoints(location.DefaultBackend, &sp, kube_api.ProtocolTCP, &healthcheck.Upstream{})
							if len(endps) > 0 {
								glog.V(3).Infof("using custom default backend in server %v location %v (service %v/%v)",
									server.Hostname, location.Path, location.DefaultBackend.Namespace, location.DefaultBackend.Name)
								b, err := cloner.DeepCopy(upstream)
								if err != nil {
									glog.Errorf("unexpected error copying Upstream: %v", err)
								} else {
									name := fmt.Sprintf("custom-default-backend-%v", upstream.Name)
									nb := b.(*ingress.Backend)
									nb.Name = name
									nb.Endpoints = endps
									aUpstreams = append(aUpstreams, nb)
									location.Backend = name
								}
							}
						}
					}

					// Configure Backends[].SSLPassthrough
					if server.SSLPassthrough {
						if location.Path == rootLocation {
							if location.Backend == defUpstreamName {
								glog.Warningf("ignoring ssl passthrough of %v as it doesn't have a default backend (root context)", server.Hostname)
								continue
							}

							isHTTPSfrom = append(isHTTPSfrom, server)
						}
					}
				}
			}
		}

		if len(isHTTPSfrom) > 0 {
			upstream.SSLPassthrough = true
		}
	}

	// create the list of upstreams and skip those without endpoints
	for _, upstream := range upstreams {
		if len(upstream.Endpoints) == 0 {
			continue
		}
		aUpstreams = append(aUpstreams, upstream)
	}

	if ic.cfg.SortBackends {
		sort.SliceStable(aUpstreams, func(a, b int) bool {
			return aUpstreams[a].Name < aUpstreams[b].Name
		})
	}

	aServers := make([]*ingress.Server, 0, len(servers))
	for _, value := range servers {
		sort.SliceStable(value.Locations, func(i, j int) bool {
			return value.Locations[i].Path > value.Locations[j].Path
		})
		aServers = append(aServers, value)
	}

	sort.SliceStable(aServers, func(i, j int) bool {
		return aServers[i].Hostname < aServers[j].Hostname
	})

	return aUpstreams, aServers
}

// GetAuthCertificate ...
func (ic GenericController) GetAuthCertificate(secretName string) (*resolver.AuthSSLCert, error) {
	if _, exists := ic.sslCertTracker.Get(secretName); !exists {
		ic.syncSecret(secretName)
	}

	_, err := ic.listers.Secret.GetByName(secretName)
	if err != nil {
		return &resolver.AuthSSLCert{}, fmt.Errorf("unexpected error: %v", err)
	}

	bc, exists := ic.sslCertTracker.Get(secretName)
	if !exists {
		return &resolver.AuthSSLCert{}, fmt.Errorf("secret %v does not exist", secretName)
	}
	cert := bc.(*ingress.SSLCert)
	return &resolver.AuthSSLCert{
		Secret:     secretName,
		CAFileName: cert.CAFileName,
		PemSHA:     cert.PemSHA,
	}, nil
}

// createUpstreams creates the NGINX upstreams for each service referenced in
// Ingress rules. The servers inside the upstream are endpoints.
func (ic *GenericController) createUpstreams(data []*extensions.Ingress, du *ingress.Backend) map[string]*ingress.Backend {
	upstreams := make(map[string]*ingress.Backend)
	upstreams[defUpstreamName] = du

	for _, ing := range data {
		secUpstream := ic.annotations.SecureUpstream(ing)
		hz := ic.annotations.HealthCheck(ing)
		serviceUpstream := ic.annotations.ServiceUpstream(ing)

		var defBackend string
		if ing.Spec.Backend != nil {
			defBackend = fmt.Sprintf("%v-%v-%v",
				ing.GetNamespace(),
				ing.Spec.Backend.ServiceName,
				ing.Spec.Backend.ServicePort.String())

			glog.V(3).Infof("creating upstream %v", defBackend)
			upstreams[defBackend] = newUpstream(defBackend)
			svcKey := fmt.Sprintf("%v/%v", ing.GetNamespace(), ing.Spec.Backend.ServiceName)

			// Add the service cluster endpoint as the upstream instead of individual endpoints
			// if the serviceUpstream annotation is enabled
			if serviceUpstream {
				endpoint, err := ic.getServiceClusterEndpoint(svcKey, ing.Spec.Backend)
				if err != nil {
					glog.Errorf("Failed to get service cluster endpoint for service %s: %v", svcKey, err)
				} else {
					upstreams[defBackend].Endpoints = []ingress.Endpoint{endpoint}
				}
			}

			if len(upstreams[defBackend].Endpoints) == 0 {
				endps, err := ic.serviceEndpoints(svcKey, ing.Spec.Backend.ServicePort.String(), hz)
				upstreams[defBackend].Endpoints = append(upstreams[defBackend].Endpoints, endps...)
				if err != nil {
					glog.Warningf("error creating upstream %v: %v", defBackend, err)
				}
			}

		}

		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}

			for _, path := range rule.HTTP.Paths {
				name := fmt.Sprintf("%v-%v-%v",
					ing.GetNamespace(),
					path.Backend.ServiceName,
					path.Backend.ServicePort.String())

				if _, ok := upstreams[name]; ok {
					continue
				}

				glog.V(3).Infof("creating upstream %v", name)
				upstreams[name] = newUpstream(name)
				upstreams[name].Port = path.Backend.ServicePort

				if !upstreams[name].Secure {
					upstreams[name].Secure = secUpstream.Secure
				}

				if upstreams[name].SecureCACert.Secret == "" {
					upstreams[name].SecureCACert = secUpstream.CACert
				}

				svcKey := fmt.Sprintf("%v/%v", ing.GetNamespace(), path.Backend.ServiceName)

				// Add the service cluster endpoint as the upstream instead of individual endpoints
				// if the serviceUpstream annotation is enabled
				if serviceUpstream {
					endpoint, err := ic.getServiceClusterEndpoint(svcKey, &path.Backend)
					if err != nil {
						glog.Errorf("failed to get service cluster endpoint for service %s: %v", svcKey, err)
					} else {
						upstreams[name].Endpoints = []ingress.Endpoint{endpoint}
					}
				}

				if len(upstreams[name].Endpoints) == 0 {
					endp, err := ic.serviceEndpoints(svcKey, path.Backend.ServicePort.String(), hz)
					if err != nil {
						glog.Warningf("error obtaining service endpoints: %v", err)
						continue
					}
					upstreams[name].Endpoints = endp
				}

				s, err := ic.listers.Service.GetByName(svcKey)
				if err != nil {
					glog.Warningf("error obtaining service: %v", err)
					continue
				}

				upstreams[name].Service = s
			}
		}
	}

	return upstreams
}

func (ic *GenericController) getServiceClusterEndpoint(svcKey string, backend *extensions.IngressBackend) (endpoint ingress.Endpoint, err error) {
	svcObj, svcExists, err := ic.listers.Service.GetByKey(svcKey)

	if !svcExists {
		return endpoint, fmt.Errorf("service %v does not exist", svcKey)
	}

	svc := svcObj.(*kube_api.Service)
	if svc.Spec.ClusterIP == "" {
		return endpoint, fmt.Errorf("No ClusterIP found for service %s", svcKey)
	}

	endpoint.Address = svc.Spec.ClusterIP
	endpoint.Port = backend.ServicePort.String()

	return endpoint, err
}

// serviceEndpoints returns the upstream servers (endpoints) associated
// to a service.
func (ic *GenericController) serviceEndpoints(svcKey, backendPort string,
	hz *healthcheck.Upstream) ([]ingress.Endpoint, error) {
	svc, err := ic.listers.Service.GetByName(svcKey)

	var upstreams []ingress.Endpoint
	if err != nil {
		return upstreams, fmt.Errorf("error getting service %v from the cache: %v", svcKey, err)
	}

	glog.V(3).Infof("obtaining port information for service %v", svcKey)
	for _, servicePort := range svc.Spec.Ports {
		// targetPort could be a string, use the name or the port (int)
		if strconv.Itoa(int(servicePort.Port)) == backendPort ||
			servicePort.TargetPort.String() == backendPort ||
			servicePort.Name == backendPort {

			endps := ic.getEndpoints(svc, &servicePort, kube_api.ProtocolTCP, hz)
			if len(endps) == 0 {
				glog.Warningf("service %v does not have any active endpoints", svcKey)
			}

			if ic.cfg.SortBackends {
				sort.SliceStable(endps, func(i, j int) bool {
					iName := endps[i].Address
					jName := endps[j].Address
					if iName != jName {
						return iName < jName
					}

					return endps[i].Port < endps[j].Port
				})
			}
			upstreams = append(upstreams, endps...)
			break
		}
	}

	if !ic.cfg.SortBackends {
		rand.Seed(time.Now().UnixNano())
		for i := range upstreams {
			j := rand.Intn(i + 1)
			upstreams[i], upstreams[j] = upstreams[j], upstreams[i]
		}
	}

	return upstreams, nil
}

// createServers initializes a map that contains information about the list of
// FDQN referenced by ingress rules and the common name field in the referenced
// SSL certificates. Each server is configured with location / using a default
// backend specified by the user or the one inside the ingress spec.
func (ic *GenericController) createServers(data []*extensions.Ingress,
	upstreams map[string]*ingress.Backend,
	du *ingress.Backend) map[string]*ingress.Server {

	servers := make(map[string]*ingress.Server, len(data))
	// If a server has a hostname equivalent to a pre-existing alias, then we
	// remove the alias to avoid conflicts.
	aliases := make(map[string]string, len(data))

	bdef := ic.GetDefaultBackend()
	ngxProxy := &proxy.Configuration{
		BodySize:         bdef.ProxyBodySize,
		ConnectTimeout:   bdef.ProxyConnectTimeout,
		SendTimeout:      bdef.ProxySendTimeout,
		ReadTimeout:      bdef.ProxyReadTimeout,
		BufferSize:       bdef.ProxyBufferSize,
		CookieDomain:     bdef.ProxyCookieDomain,
		CookiePath:       bdef.ProxyCookiePath,
		NextUpstream:     bdef.ProxyNextUpstream,
		RequestBuffering: bdef.ProxyRequestBuffering,
	}

	defaultPemFileName := fakeCertificatePath
	defaultPemSHA := fakeCertificateSHA

	// Tries to fetch the default Certificate. If it does not exists, generate a new self signed one.
	defaultCertificate, err := ic.getPemCertificate(ic.cfg.DefaultSSLCertificate)
	if err == nil {
		defaultPemFileName = defaultCertificate.PemFileName
		defaultPemSHA = defaultCertificate.PemSHA
	}

	// initialize the default server
	servers[defServerName] = &ingress.Server{
		Hostname:       defServerName,
		SSLCertificate: defaultPemFileName,
		SSLPemChecksum: defaultPemSHA,
		Locations: []*ingress.Location{
			{
				Path:         rootLocation,
				IsDefBackend: true,
				Backend:      du.Name,
				Proxy:        ngxProxy,
				Service:      du.Service,
			},
		}}

	// initialize all the servers
	for _, ing := range data {

		// check if ssl passthrough is configured
		sslpt := ic.annotations.SSLPassthrough(ing)

		// default upstream server
		un := du.Name

		if ing.Spec.Backend != nil {
			// replace default backend
			defUpstream := fmt.Sprintf("%v-%v-%v", ing.GetNamespace(), ing.Spec.Backend.ServiceName, ing.Spec.Backend.ServicePort.String())
			if backendUpstream, ok := upstreams[defUpstream]; ok {
				un = backendUpstream.Name

				// Special case:
				// ingress only with a backend and no rules
				// this case defines a "catch all" server
				defLoc := servers[defServerName].Locations[0]
				if defLoc.IsDefBackend && len(ing.Spec.Rules) == 0 {
					defLoc.IsDefBackend = false
					defLoc.Backend = backendUpstream.Name
					defLoc.Service = backendUpstream.Service
				}
			}
		}

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}
			if _, ok := servers[host]; ok {
				// server already configured
				continue
			}

			servers[host] = &ingress.Server{
				Hostname: host,
				Locations: []*ingress.Location{
					{
						Path:         rootLocation,
						IsDefBackend: true,
						Backend:      un,
						Proxy:        ngxProxy,
						Service:      &kube_api.Service{},
					},
				},
				SSLPassthrough: sslpt,
			}
		}
	}

	// configure default location, alias, and SSL
	for _, ing := range data {
		// setup server-alias based on annotations
		aliasAnnotation := ic.annotations.Alias(ing)

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}

			// setup server aliases
			servers[host].Alias = aliasAnnotation
			if aliasAnnotation != "" {
				if _, ok := aliases[aliasAnnotation]; !ok {
					aliases[aliasAnnotation] = host
				}
			}

			// only add a certificate if the server does not have one previously configured
			if servers[host].SSLCertificate != "" {
				continue
			}

			if len(ing.Spec.TLS) == 0 {
				glog.V(3).Infof("ingress %v/%v for host %v does not contains a TLS section", ing.Namespace, ing.Name, host)
				continue
			}

			tlsSecretName := ""
			found := false
			for _, tls := range ing.Spec.TLS {
				if sets.NewString(tls.Hosts...).Has(host) {
					tlsSecretName = tls.SecretName
					found = true
					break
				}
			}

			if !found {
				glog.Warningf("ingress %v/%v for host %v contains a TLS section but none of the host match",
					ing.Namespace, ing.Name, host)
				continue
			}

			if tlsSecretName == "" {
				glog.V(3).Infof("host %v is listed on tls section but secretName is empty. Using default cert", host)
				servers[host].SSLCertificate = defaultPemFileName
				servers[host].SSLPemChecksum = defaultPemSHA
				continue
			}

			key := fmt.Sprintf("%v/%v", ing.Namespace, tlsSecretName)
			bc, exists := ic.sslCertTracker.Get(key)
			if !exists {
				glog.Warningf("ssl certificate \"%v\" does not exist in local store", key)
				continue
			}

			cert := bc.(*ingress.SSLCert)
			err = cert.Certificate.VerifyHostname(host)
			if err != nil {
				glog.Warningf("ssl certificate %v does not contain a Common Name or Subject Alternative Name for host %v", key, host)
				continue
			}

			servers[host].SSLCertificate = cert.PemFileName
			servers[host].SSLPemChecksum = cert.PemSHA
			servers[host].SSLExpireTime = cert.ExpireTime

			if cert.ExpireTime.Before(time.Now().Add(240 * time.Hour)) {
				glog.Warningf("ssl certificate for host %v is about to expire in 10 days", host)
			}
		}
	}

	for alias, host := range aliases {
		if _, ok := servers[alias]; ok {
			glog.Warningf("There is a conflict with server hostname '%v' and alias '%v' (in server %v). Removing alias to avoid conflicts.", alias, host)
			servers[host].Alias = ""
		}
	}
	return servers
}

// getEndpoints returns a list of <endpoint ip>:<port> for a given service/target port combination.
func (ic *GenericController) getEndpoints(
	s *kube_api.Service,
	servicePort *kube_api.ServicePort,
	proto kube_api.Protocol,
	hz *healthcheck.Upstream) []ingress.Endpoint {

	upsServers := []ingress.Endpoint{}
	fmt.Println("-------------test")
	requirement, err := labels.NewRequirement("app", selection.Equals, []string{"apptest"})
	fmt.Println(err)
	pods, err := ic.listers.Pod.Pods("default").List(labels.NewSelector().Add(*requirement))
	data := make(map[string]*heapsterManager.MetricSet2)
	datasum := *ic.heapsterManager.DataSum()
	for i, _ := range pods {
		for key, value := range datasum {
			if strings.Contains(key, pods[i].GetName()) {
				data[pods[i].Status.PodIP] = value
				continue
			}
		}
	}
	for key, value := range data {
		fmt.Println("key: ", key)
		fmt.Println("value: ", value)
	}

	fmt.Println("--------------test-ok")

	// avoid duplicated upstream servers when the service
	// contains multiple port definitions sharing the same
	// targetport.
	adus := make(map[string]bool)

	//随意加入一个weight
	weight := 5

	// ExternalName services
	if s.Spec.Type == kube_api.ServiceTypeExternalName {
		targetPort := servicePort.TargetPort.IntValue()
		// check for invalid port value
		if targetPort <= 0 {
			return upsServers
		}

		if net.ParseIP(s.Spec.ExternalName) == nil {
			_, err := net.LookupHost(s.Spec.ExternalName)
			if err != nil {
				glog.Errorf("unexpected error resolving host %v: %v", s.Spec.ExternalName, err)
				return upsServers
			}
		}

		return append(upsServers, ingress.Endpoint{
			Address:     s.Spec.ExternalName,
			Port:        fmt.Sprintf("%v", targetPort),
			MaxFails:    hz.MaxFails,
			FailTimeout: hz.FailTimeout,
			//在这里加入权重
			Weight: weight,
		})
	}

	glog.V(3).Infof("getting endpoints for service %v/%v and port %v", s.Namespace, s.Name, servicePort.String())
	ep, err := ic.listers.Endpoint.GetServiceEndpoints(s)
	if err != nil {
		glog.Warningf("unexpected error obtaining service endpoints: %v", err)
		return upsServers
	}

	for _, ss := range ep.Subsets {
		for _, epPort := range ss.Ports {

			if !reflect.DeepEqual(epPort.Protocol, proto) {
				continue
			}

			var targetPort int32

			if servicePort.Name == "" {
				// ServicePort.Name is optional if there is only one port
				targetPort = epPort.Port
			} else if servicePort.Name == epPort.Name {
				targetPort = epPort.Port
			}

			// check for invalid port value
			if targetPort <= 0 {
				continue
			}

			for _, epAddress := range ss.Addresses {
				ep := fmt.Sprintf("%v:%v", epAddress.IP, targetPort)
				if _, exists := adus[ep]; exists {
					continue
				}
				ups := ingress.Endpoint{
					Address:     epAddress.IP,
					Port:        fmt.Sprintf("%v", targetPort),
					MaxFails:    hz.MaxFails,
					FailTimeout: hz.FailTimeout,
					//在这里加入权重
					Weight: weight,
					Target: epAddress.TargetRef,
				}
				upsServers = append(upsServers, ups)
				adus[ep] = true
			}
		}
	}

	glog.V(3).Infof("endpoints found: %v", upsServers)
	return upsServers
}

// readSecrets extracts information about secrets from an Ingress rule
func (ic *GenericController) readSecrets(ing *extensions.Ingress) {
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			continue
		}

		key := fmt.Sprintf("%v/%v", ing.Namespace, tls.SecretName)
		ic.syncSecret(key)
	}

	key, _ := parser.GetStringAnnotation("ingress.kubernetes.io/auth-tls-secret", ing)
	if key == "" {
		return
	}
	ic.syncSecret(key)
}

// Stop stops the loadbalancer controller.
func (ic GenericController) Stop() error {
	ic.stopLock.Lock()
	//heapster manager的停止
	ic.heapsterManager.Stop()

	defer ic.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !ic.syncQueue.IsShuttingDown() {
		glog.Infof("shutting down controller queues")
		close(ic.stopCh)
		go ic.syncQueue.Shutdown()
		if ic.syncStatus != nil {
			ic.syncStatus.Shutdown()
		}
		return nil
	}

	return fmt.Errorf("shutdown already in progress")
}

// Start starts the Ingress controller.
func (ic *GenericController) Start() {
	glog.Infof("starting Ingress controller")

	//heapster manager的启动
	ic.heapsterManager.Start()

	go ic.ingController.Run(ic.stopCh)
	go ic.endpController.Run(ic.stopCh)
	go ic.svcController.Run(ic.stopCh)
	go ic.nodeController.Run(ic.stopCh)
	go ic.secrController.Run(ic.stopCh)
	go ic.mapController.Run(ic.stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(ic.stopCh,
		ic.ingController.HasSynced,
		ic.svcController.HasSynced,
		ic.endpController.HasSynced,
		ic.secrController.HasSynced,
		ic.mapController.HasSynced,
		ic.nodeController.HasSynced,
	) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
	}

	// initial sync of secrets to avoid unnecessary reloads
	for _, key := range ic.listers.Ingress.ListKeys() {
		if obj, exists, _ := ic.listers.Ingress.GetByKey(key); exists {
			ing := obj.(*extensions.Ingress)

			if !class.IsValid(ing, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
				a, _ := parser.GetStringAnnotation(class.IngressKey, ing)
				glog.Infof("ignoring add for ingress %v based on annotation %v with value %v", ing.Name, class.IngressKey, a)
				continue
			}

			ic.readSecrets(ing)
		}
	}

	createDefaultSSLCertificate()

	ic.setInitialSyncDone()

	go ic.syncQueue.Run(time.Second, ic.stopCh)

	if ic.syncStatus != nil {
		go ic.syncStatus.Run(ic.stopCh)
	}

	time.Sleep(5 * time.Second)
	// force initial sync
	ic.syncQueue.Enqueue(&extensions.Ingress{})

	<-ic.stopCh
}

func (ic *GenericController) isForceReload() bool {
	return atomic.LoadInt32(&ic.forceReload) != 0
}

func (ic *GenericController) setForceReload(shouldReload bool) {
	if shouldReload {
		atomic.StoreInt32(&ic.forceReload, 1)
	} else {
		atomic.StoreInt32(&ic.forceReload, 0)
	}
}

func (ic *GenericController) isInitialSyncDone() bool {
	return atomic.LoadInt32(&ic.initialSyncDone) != 0
}

func (ic *GenericController) setInitialSyncDone() {
	atomic.StoreInt32(&ic.initialSyncDone, 1)
}

func createDefaultSSLCertificate() {
	defCert, defKey := ssl.GetFakeSSLCert()
	c, err := ssl.AddOrUpdateCertAndKey(fakeCertificate, defCert, defKey, []byte{})
	if err != nil {
		glog.Fatalf("Error generating self signed certificate: %v", err)
	}

	fakeCertificateSHA = c.PemSHA
	fakeCertificatePath = c.PemFileName
}
