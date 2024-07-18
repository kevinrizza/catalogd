/*
Copyright 2022.

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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/operator-framework/catalogd/api/core/v1alpha1"
	corecontrollers "github.com/operator-framework/catalogd/internal/controllers/core"
	"github.com/operator-framework/catalogd/internal/features"
	"github.com/operator-framework/catalogd/internal/garbagecollection"
	catalogdmetrics "github.com/operator-framework/catalogd/internal/metrics"
	"github.com/operator-framework/catalogd/internal/serverutil"
	"github.com/operator-framework/catalogd/internal/source"
	"github.com/operator-framework/catalogd/internal/storage"
	"github.com/operator-framework/catalogd/internal/version"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const storageDir = "catalogs"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr          string
		enableLeaderElection bool
		probeAddr            string
		pprofAddr            string
		catalogdVersion      bool
		systemNamespace      string
		catalogServerAddr    string
		externalAddr         string
		cacheDir             string
		gcInterval           time.Duration
		certFile             string
		keyFile              string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&pprofAddr, "pprof-bind-address", "0", "The address the pprof endpoint binds to. an empty string or 0 disables pprof")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&systemNamespace, "system-namespace", "", "The namespace catalogd uses for internal state, configuration, and workloads")
	flag.StringVar(&catalogServerAddr, "catalogs-server-addr", ":8083", "The address where the unpacked catalogs' content will be accessible")
	flag.StringVar(&externalAddr, "external-address", "catalogd-catalogserver.olmv1-system.svc", "The external address at which the http(s) server is reachable.")
	flag.StringVar(&cacheDir, "cache-dir", "/var/cache/", "The directory in the filesystem that catalogd will use for file based caching")
	flag.BoolVar(&catalogdVersion, "version", false, "print the catalogd version and exit")
	flag.DurationVar(&gcInterval, "gc-interval", 12*time.Hour, "interval in which garbage collection should be run against the catalog content cache")
	flag.StringVar(&certFile, "tls-cert", "", "The certificate file used for serving catalog contents over HTTPS. Requires tls-key.")
	flag.StringVar(&keyFile, "tls-key", "", "The key file used for serving catalog contents over HTTPS. Requires tls-cert.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)

	// Combine both flagsets and parse them
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	features.CatalogdFeatureGate.AddFlag(pflag.CommandLine)
	pflag.Parse()

	if catalogdVersion {
		fmt.Printf("%#v\n", version.Version())
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if (certFile != "" && keyFile == "") || (certFile == "" && keyFile != "") {
		setupLog.Error(nil, "unable to configure TLS certificates: tls-cert and tls-key flags must be used together")
		os.Exit(1)
	}

	protocol := "http://"
	if certFile != "" && keyFile != "" {
		protocol = "https://"
	}
	externalAddr = protocol + externalAddr

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		PprofBindAddress:       pprofAddr,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "catalogd-operator-lock",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if systemNamespace == "" {
		systemNamespace = podNamespace()
	}

	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		setupLog.Error(err, "unable to create cache directory")
		os.Exit(1)
	}

	unpacker, err := source.NewDefaultUnpacker(systemNamespace, cacheDir)
	if err != nil {
		setupLog.Error(err, "unable to create unpacker")
		os.Exit(1)
	}

	var localStorage storage.Instance
	metrics.Registry.MustRegister(catalogdmetrics.RequestDurationMetric)

	storeDir := filepath.Join(cacheDir, storageDir)
	if err := os.MkdirAll(storeDir, 0700); err != nil {
		setupLog.Error(err, "unable to create storage directory for catalogs")
		os.Exit(1)
	}

	baseStorageURL, err := url.Parse(fmt.Sprintf("%s/catalogs/", externalAddr))
	if err != nil {
		setupLog.Error(err, "unable to create base storage URL")
		os.Exit(1)
	}

	localStorage = storage.LocalDir{RootDir: storeDir, BaseURL: baseStorageURL}

	// Get a config to set configmap metadata outside of manager
	k8sclient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to generate client from config")
	}
	connectionDetailsEnsurer := serverutil.ConnectionDetailsEnsurer{
		Client:           k8sclient,
		ServiceName:      "catalogd-catalogserver",
		ServiceNamespace: "olmv1-system",
	}

	tlsFileWatcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		setupLog.Error(err, "error creating TLS certificate watcher")
	}
	tlsFileWatcher.RegisterCallback(func(tls.Certificate) {
		err = connectionDetailsEnsurer.EnsureConnectionDetailsConfigMap(certFile)
		if err != nil {
			setupLog.Error(err, "error ensuring catalogd service connection details configmap")
		}
	})

	catalogServerConfig := serverutil.CatalogServerConfig{
		ExternalAddr:   externalAddr,
		CatalogAddr:    catalogServerAddr,
		CertFile:       certFile,
		KeyFile:        keyFile,
		LocalStorage:   localStorage,
		TlsFileWatcher: tlsFileWatcher,
	}

	err = serverutil.AddCatalogServerToManager(mgr, catalogServerConfig)
	if err != nil {
		setupLog.Error(err, "unable to configure catalog server")
		os.Exit(1)
	}

	if err = (&corecontrollers.ClusterCatalogReconciler{
		Client:   mgr.GetClient(),
		Unpacker: unpacker,
		Storage:  localStorage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterCatalog")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	metaClient, err := metadata.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to setup client for garbage collection")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	gc := &garbagecollection.GarbageCollector{
		CachePath:      filepath.Join(cacheDir, source.UnpackCacheDir),
		Logger:         ctrl.Log.WithName("garbage-collector"),
		MetadataClient: metaClient,
		Interval:       gcInterval,
	}
	if err := mgr.Add(gc); err != nil {
		setupLog.Error(err, "unable to add garbage collector to manager")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func podNamespace() string {
	namespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "olmv1-system"
	}
	return string(namespace)
}
