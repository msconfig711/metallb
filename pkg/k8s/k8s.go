// SPDX-License-Identifier:Apache-2.0

package k8s // import "go.universe.tf/metallb/pkg/k8s"

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	metallbv1alpha1 "go.universe.tf/metallb/api/v1alpha1"
	metallbv1beta1 "go.universe.tf/metallb/api/v1beta1"
	"go.universe.tf/metallb/pkg/config"
	"go.universe.tf/metallb/pkg/k8s/controllers"
	"go.universe.tf/metallb/pkg/k8s/epslices"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	policyv1beta1 "k8s.io/kubernetes/pkg/apis/policy/v1beta1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	rbacv1 "k8s.io/kubernetes/pkg/apis/rbac/v1"

	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(metallbv1alpha1.AddToScheme(scheme))
	utilruntime.Must(metallbv1beta1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(policyv1beta1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
	utilruntime.Must(apiext.AddToScheme(scheme))
	utilruntime.Must(discovery.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

// Client watches a Kubernetes cluster and translates events into
// Controller method calls.
type Client struct {
	logger log.Logger

	client         *kubernetes.Clientset
	events         record.EventRecorder
	mgr            manager.Manager
	reloader       *controllers.ServiceReloadReconciler
	validateConfig config.Validate
	ForceSync      func()
}

// Config specifies the configuration of the Kubernetes
// client/watcher.
type Config struct {
	ProcessName     string
	NodeName        string
	MetricsHost     string
	MetricsPort     int
	EnablePprof     bool
	ReadEndpoints   bool
	Logger          log.Logger
	DisableEpSlices bool
	Namespace       string
	ValidateConfig  config.Validate

	Listener
}

// New connects to masterAddr, using kubeconfig to authenticate.
//
// The client uses processName to identify itself to the cluster
// (e.g. when logging events).
//nolint:godot
func New(cfg *Config) (*Client, error) {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:         scheme,
		Port:           9443, // TODO port only with controller, for webhooks
		LeaderElection: false,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %s", err)
	}

	recorder := mgr.GetEventRecorderFor(cfg.ProcessName)

	reloadChan := make(chan event.GenericEvent)
	reload := func() {
		reloadChan <- controllers.NewReloadEvent()
	}

	c := &Client{
		logger:         cfg.Logger,
		client:         clientset,
		events:         recorder,
		mgr:            mgr,
		validateConfig: cfg.ValidateConfig,
		ForceSync:      reload,
	}

	if cfg.ConfigChanged != nil {
		if err = (&controllers.ConfigReconciler{
			Client:         mgr.GetClient(),
			Logger:         cfg.Logger,
			Scheme:         mgr.GetScheme(),
			Namespace:      cfg.Namespace,
			ValidateConfig: cfg.ValidateConfig,
			Handler:        cfg.ConfigHandler,
			ForceReload:    reload,
		}).SetupWithManager(mgr); err != nil {
			level.Error(c.logger).Log("error", err, "unable to create controller", "config")
			return nil, errors.Wrap(err, "failed to create config reconciler")
		}
	}

	if cfg.NodeChanged != nil {
		if err = (&controllers.NodeReconciler{
			Client:  mgr.GetClient(),
			Logger:  cfg.Logger,
			Scheme:  mgr.GetScheme(),
			Handler: cfg.NodeHandler,
		}).SetupWithManager(mgr); err != nil {
			level.Error(c.logger).Log("error", err, "unable to create controller", "node")
			return nil, errors.Wrap(err, "failed to create node reconciler")
		}
	}

	// use DisableEpSlices to skip the autodiscovery mechanism. Useful if EndpointSlices are enabled in the cluster but disabled in kube-proxy
	useSlices := UseEndpointSlices(c.client) && !cfg.DisableEpSlices

	var needEndpoints controllers.NeedEndPoints
	switch {
	case !cfg.ReadEndpoints:
		needEndpoints = controllers.NoNeed
	case useSlices:
		needEndpoints = controllers.EndpointSlices
	default:
		needEndpoints = controllers.Endpoints
	}

	if needEndpoints == controllers.EndpointSlices {
		// Set a field indexer so we can retrieve all the endpoints for a given service.
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &discovery.EndpointSlice{}, epslices.SlicesServiceIndexName, func(rawObj client.Object) []string {
			epSlice, ok := rawObj.(*discovery.EndpointSlice)
			if epSlice == nil {
				level.Error(c.logger).Log("controller", "fieldindexer", "error", "received nil epslice")
				return nil
			}
			if !ok {
				level.Error(c.logger).Log("controller", "fieldindexer", "error", "received object that is not epslice", "object", rawObj.GetObjectKind().GroupVersionKind().Kind)
				return nil
			}
			serviceKey, err := epslices.ServiceKeyForSlice(epSlice)
			if err != nil {
				level.Error(c.logger).Log("controller", "ServiceReconciler", "error", "failed to get service from epslices", "epslice", epSlice.Name, "error", err)
			}
			return []string{serviceKey.String()}
		}); err != nil {
			return nil, err
		}
	}

	if cfg.ServiceChanged != nil {
		if err = (&controllers.ServiceReconciler{
			Client:      mgr.GetClient(),
			Logger:      cfg.Logger,
			Scheme:      mgr.GetScheme(),
			Handler:     cfg.ServiceHandler,
			Endpoints:   needEndpoints,
			ForceReload: reload,
		}).SetupWithManager(mgr); err != nil {
			level.Error(c.logger).Log("error", err, "unable to create controller", "service")
			return nil, errors.Wrap(err, "failed to create service reconciler")
		}
	}

	c.reloader = &controllers.ServiceReloadReconciler{
		Client:    mgr.GetClient(),
		Log:       cfg.Logger,
		Scheme:    mgr.GetScheme(),
		Handler:   cfg.ServiceHandler,
		Endpoints: needEndpoints,
		Reload:    reloadChan,
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if cfg.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	go func(l log.Logger) {
		err := http.ListenAndServe(net.JoinHostPort(cfg.MetricsHost, fmt.Sprint(cfg.MetricsPort)), mux)
		if err != nil {
			level.Error(l).Log("op", "listenAndServe", "err", err, "msg", "cannot listen and serve", "host", cfg.MetricsHost, "port", cfg.MetricsPort)
		}
	}(c.logger)

	return c, nil
}

// CreateMlSecret create the memberlist secret.
func (c *Client) CreateMlSecret(namespace, controllerDeploymentName, secretName string) error {
	// Use List instead of Get to differentiate between API errors and non existing secret.
	// Matching error text is prone to future breakage.
	l, err := c.client.CoreV1().Secrets(namespace).List(context.TODO(), metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", secretName).String(),
	})
	if err != nil {
		return err
	}
	if len(l.Items) > 0 {
		level.Debug(c.logger).Log("op", "CreateMlSecret", "msg", "secret already exists, nothing to do")
		return nil
	}

	// Get the controller Deployment info to set secret ownerReference.
	d, err := c.client.AppsV1().Deployments(namespace).Get(context.TODO(), controllerDeploymentName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// Create the secret key (128 bits).
	secret := make([]byte, 16)
	_, err = rand.Read(secret)
	if err != nil {
		return err
	}
	// base64 encode the secret key as it'll be passed a env variable.
	secretB64 := make([]byte, base64.RawStdEncoding.EncodedLen(len(secret)))
	base64.RawStdEncoding.Encode(secretB64, secret)

	// Create the K8S Secret object.
	_, err = c.client.CoreV1().Secrets(namespace).Create(
		context.TODO(),
		&v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: secretName,
				OwnerReferences: []metav1.OwnerReference{{
					// d.APIVersion is empty.
					APIVersion: "apps/v1",
					// d.Kind is empty.
					Kind: "Deployment",
					Name: d.Name,
					UID:  d.UID,
				}},
			},
			Data: map[string][]byte{"secretkey": secretB64},
		},
		metav1.CreateOptions{})
	if err == nil {
		level.Info(c.logger).Log("op", "CreateMlSecret", "msg", "secret succesfully created")
	}
	return err
}

// PodIPs returns the IPs of all the pods matched by the labels string.
func (c *Client) PodIPs(namespace, labels string) ([]string, error) {
	pl, err := c.client.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labels})
	if err != nil {
		return nil, err
	}
	iplist := []string{}
	for _, pod := range pl.Items {
		iplist = append(iplist, pod.Status.PodIP)
	}
	return iplist, nil
}

// Run watches for events on the Kubernetes cluster, and dispatches
// calls to the Controller.
func (c *Client) Run(stopCh <-chan struct{}) error {
	ctx := ctrl.SetupSignalHandler()

	level.Info(c.logger).Log("Starting Reloader")
	if err := c.reloader.Start(ctx, c.mgr); err != nil {
		level.Error(c.logger).Log("error", err, "unable to start", "reloader")
		return errors.Wrap(err, "failed to start reloader")
	}
	level.Info(c.logger).Log("Starting Manager")
	if err := c.mgr.Start(ctx); err != nil {
		return err
	}

	return nil
}

// UpdateStatus writes the protected "status" field of svc back into
// the Kubernetes cluster.
func (c *Client) UpdateStatus(svc *v1.Service) error {
	_, err := c.client.CoreV1().Services(svc.Namespace).UpdateStatus(context.TODO(), svc, metav1.UpdateOptions{})
	return err
}

// Infof logs an informational event about svc to the Kubernetes cluster.
func (c *Client) Infof(svc *v1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, v1.EventTypeNormal, kind, msg, args...)
}

// Errorf logs an error event about svc to the Kubernetes cluster.
func (c *Client) Errorf(svc *v1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, v1.EventTypeWarning, kind, msg, args...)
}

// UseEndpointSlices detect if Endpoints Slices are enabled in the cluster.
func UseEndpointSlices(kubeClient kubernetes.Interface) bool {
	if _, err := kubeClient.Discovery().ServerResourcesForGroupVersion(discovery.SchemeGroupVersion.String()); err != nil {
		return false
	}
	// this is needed to check if ep slices are enabled on the cluster. In 1.17 the resources are there but disabled by default
	if _, err := kubeClient.DiscoveryV1beta1().EndpointSlices("default").Get(context.Background(), "kubernetes", metav1.GetOptions{}); err != nil {
		return false
	}
	return true
}
