package controller

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/wy2745/ingress/core/pkg/ingress"
	"github.com/wy2745/ingress/core/pkg/k8s"
)

// NewIngressController returns a configured Ingress controller
func NewIngressController(backend ingress.Controller) *GenericController {
	var (
		flags = pflag.NewFlagSet("", pflag.ExitOnError)

		apiserverHost = flags.String("apiserver-host", "", "The address of the Kubernetes Apiserver "+
			"to connect to in the format of protocol://address:port, e.g., "+
			"http://localhost:8080. If not specified, the assumption is that the binary runs inside a "+
			"Kubernetes cluster and local discovery is attempted.")
		kubeConfigFile = flags.String("kubeconfig", "", "Path to kubeconfig file with authorization and master location information.")

		defaultSvc = flags.String("default-backend-service", "",
			`Service used to serve a 404 page for the default backend. Takes the form
    	namespace/name. The controller uses the first node port of this Service for
    	the default backend.`)

		ingressClass = flags.String("ingress-class", "",
			`Name of the ingress class to route through this controller.`)

		configMap = flags.String("configmap", "",
			`Name of the ConfigMap that contains the custom configuration to use`)

		publishSvc = flags.String("publish-service", "",
			`Service fronting the ingress controllers. Takes the form
 		namespace/name. The controller will set the endpoint records on the
 		ingress objects to reflect those on the service.`)

		tcpConfigMapName = flags.String("tcp-services-configmap", "",
			`Name of the ConfigMap that contains the definition of the TCP services to expose.
		The key in the map indicates the external port to be used. The value is the name of the
		service with the format namespace/serviceName and the port of the service could be a
		number of the name of the port.
		The ports 80 and 443 are not allowed as external ports. This ports are reserved for the backend`)

		udpConfigMapName = flags.String("udp-services-configmap", "",
			`Name of the ConfigMap that contains the definition of the UDP services to expose.
		The key in the map indicates the external port to be used. The value is the name of the
		service with the format namespace/serviceName and the port of the service could be a
		number of the name of the port.`)

		resyncPeriod = flags.Duration("sync-period", 600*time.Second,
			`Relist and confirm cloud resources this often. Default is 10 minutes`)

		watchNamespace = flags.String("watch-namespace", apiv1.NamespaceAll,
			`Namespace to watch for Ingress. Default is to watch all namespaces`)

		healthzPort = flags.Int("healthz-port", 10254, "port for healthz endpoint.")

		profiling = flags.Bool("profiling", true, `Enable profiling via web interface host:port/debug/pprof/`)

		defSSLCertificate = flags.String("default-ssl-certificate", "", `Name of the secret
		that contains a SSL certificate to be used as default for a HTTPS catch-all server`)

		defHealthzURL = flags.String("health-check-path", "/healthz", `Defines
		the URL to be used as health check inside in the default server in NGINX.`)

		updateStatus = flags.Bool("update-status", true, `Indicates if the
		ingress controller should update the Ingress status IP/hostname. Default is true`)

		electionID = flags.String("election-id", "ingress-controller-leader", `Election id to use for status update.`)

		forceIsolation = flags.Bool("force-namespace-isolation", false,
			`Force namespace isolation. This flag is required to avoid the reference of secrets or
		configmaps located in a different namespace than the specified in the flag --watch-namespace.`)

		disableNodeList = flags.Bool("disable-node-list", false,
			`Disable querying nodes. If --force-namespace-isolation is true, this should also be set.`)

		updateStatusOnShutdown = flags.Bool("update-status-on-shutdown", true, `Indicates if the
		ingress controller should update the Ingress status IP/hostname when the controller
		is being stopped. Default is true`)

		sortBackends = flags.Bool("sort-backends", false,
			`Defines if backends and it's endpoints should be sorted`)
	)

	flags.AddGoFlagSet(flag.CommandLine)
	backend.ConfigureFlags(flags)
	flags.Parse(os.Args)
	backend.OverrideFlags(flags)

	flag.Set("logtostderr", "true")

	glog.Info(backend.Info())

	if *ingressClass != "" {
		glog.Infof("Watching for ingress class: %s", *ingressClass)
	}

	if *defaultSvc == "" {
		glog.Fatalf("Please specify --default-backend-service")
	}

	kubeClient, err := createApiserverClient(*apiserverHost, *kubeConfigFile)
	if err != nil {
		handleFatalInitError(err)
	}

	ns, name, err := k8s.ParseNameNS(*defaultSvc)
	if err != nil {
		glog.Fatalf("invalid format for service %v: %v", *defaultSvc, err)
	}

	_, err = kubeClient.Core().Services(ns).Get(name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "cannot get services in the namespace") {
			glog.Fatalf("✖ It seems the cluster it is running with Authorization enabled (like RBAC) and there is no permissions for the ingress controller. Please check the configuration")
		}
		glog.Fatalf("no service with name %v found: %v", *defaultSvc, err)
	}
	glog.Infof("validated %v as the default backend", *defaultSvc)

	if *publishSvc != "" {
		ns, name, err := k8s.ParseNameNS(*publishSvc)
		if err != nil {
			glog.Fatalf("invalid service format: %v", err)
		}

		svc, err := kubeClient.CoreV1().Services(ns).Get(name, metav1.GetOptions{})
		if err != nil {
			glog.Fatalf("unexpected error getting information about service %v: %v", *publishSvc, err)
		}

		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			if len(svc.Spec.ExternalIPs) > 0 {
				glog.Infof("service %v validated as assigned with externalIP", *publishSvc)
			} else {
				// We could poll here, but we instead just exit and rely on k8s to restart us
				glog.Fatalf("service %s does not (yet) have ingress points", *publishSvc)
			}
		} else {
			glog.Infof("service %v validated as source of Ingress status", *publishSvc)
		}
	}

	if *watchNamespace != "" {
		_, err = kubeClient.CoreV1().Namespaces().Get(*watchNamespace, metav1.GetOptions{})
		if err != nil {
			glog.Fatalf("no watchNamespace with name %v found: %v", *watchNamespace, err)
		}
	}

	if resyncPeriod.Seconds() < 10 {
		glog.Fatalf("resync period (%vs) is too low", resyncPeriod.Seconds())
	}

	err = os.MkdirAll(ingress.DefaultSSLDirectory, 0655)
	if err != nil {
		glog.Errorf("Failed to mkdir SSL directory: %v", err)
	}

	config := &Configuration{
		UpdateStatus:            *updateStatus,
		ElectionID:              *electionID,
		Client:                  kubeClient,
		//需要注意的是这里的时间，需要探究更新时间带来的影响，默认是10min
		ResyncPeriod:            *resyncPeriod,
		DefaultService:          *defaultSvc,
		IngressClass:            *ingressClass,
		DefaultIngressClass:     backend.DefaultIngressClass(),
		Namespace:               *watchNamespace,
		ConfigMapName:           *configMap,
		TCPConfigMapName:        *tcpConfigMapName,
		UDPConfigMapName:        *udpConfigMapName,
		DefaultSSLCertificate:   *defSSLCertificate,
		DefaultHealthzURL:       *defHealthzURL,
		PublishService:          *publishSvc,
		Backend:                 backend,
		ForceNamespaceIsolation: *forceIsolation,
		DisableNodeList:         *disableNodeList,
		UpdateStatusOnShutdown:  *updateStatusOnShutdown,
		SortBackends:            *sortBackends,
	}

	//使用已有的配置来建立一个新的ingress controller
	ic := newIngressController(config)
	go registerHandlers(*profiling, *healthzPort, ic)
	return ic
}

func registerHandlers(enableProfiling bool, port int, ic *GenericController) {
	mux := http.NewServeMux()
	// expose health check endpoint (/healthz)
	healthz.InstallHandler(mux,
		healthz.PingHealthz,
		ic.cfg.Backend,
	)

	mux.Handle("/metrics", promhttp.Handler())

	mux.HandleFunc("/build", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(ic.Info())
		w.Write(b)
	})

	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		if err != nil {
			glog.Errorf("unexpected error: %v", err)
		}
	})

	if enableProfiling {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%v", port),
		Handler: mux,
	}
	glog.Fatal(server.ListenAndServe())
}

const (
	// High enough QPS to fit all expected use cases. QPS=0 is not set here, because
	// client code is overriding it.
	defaultQPS = 1e6
	// High enough Burst to fit all expected use cases. Burst=0 is not set here, because
	// client code is overriding it.
	defaultBurst = 1e6
)

// buildConfigFromFlags builds REST config based on master URL and kubeconfig path.
// If both of them are empty then in cluster config is used.
func buildConfigFromFlags(masterURL, kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" && masterURL == "" {
		kubeconfig, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}

		return kubeconfig, nil
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{
			ClusterInfo: clientcmdapi.Cluster{
				Server: masterURL,
			},
		}).ClientConfig()
}

// createApiserverClient creates new Kubernetes Apiserver client. When kubeconfig or apiserverHost param is empty
// the function assumes that it is running inside a Kubernetes cluster and attempts to
// discover the Apiserver. Otherwise, it connects to the Apiserver specified.
//
// apiserverHost param is in the format of protocol://address:port/pathPrefix, e.g.http://localhost:8001.
// kubeConfig location of kubeconfig file
func createApiserverClient(apiserverHost string, kubeConfig string) (*kubernetes.Clientset, error) {
	cfg, err := buildConfigFromFlags(apiserverHost, kubeConfig)
	if err != nil {
		return nil, err
	}

	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	glog.Infof("Creating API client for %s", cfg.Host)

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	v, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, err
	}

	glog.Infof("Running in Kubernetes Cluster version v%v.%v (%v) - git (%v) commit %v - platform %v",
		v.Major, v.Minor, v.GitVersion, v.GitTreeState, v.GitCommit, v.Platform)

	return client, nil
}

/**
 * Handles fatal init error that prevents server from doing any work. Prints verbose error
 * message and quits the server.
 */
func handleFatalInitError(err error) {
	glog.Fatalf("Error while initializing connection to Kubernetes apiserver. "+
		"This most likely means that the cluster is misconfigured (e.g., it has "+
		"invalid apiserver certificates or service accounts configuration). Reason: %s\n"+
		"Refer to the troubleshooting guide for more information: "+
		"https://github.com/kubernetes/ingress/blob/master/docs/troubleshooting.md", err)
}
