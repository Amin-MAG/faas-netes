// Copyright (c) Alex Ellis 2017. All rights reserved.
// Copyright (c) OpenFaaS Author(s) 2020. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/redis/go-redis/v9"
	"log"
	"os"
	"time"

	faasProvider "github.com/Amin-MAG/faas-provider"
	"github.com/Amin-MAG/faas-provider/logs"
	"github.com/Amin-MAG/faas-provider/proxy"
	providertypes "github.com/Amin-MAG/faas-provider/types"
	clientset "github.com/openfaas/faas-netes/pkg/client/clientset/versioned"
	informers "github.com/openfaas/faas-netes/pkg/client/informers/externalversions"
	v1 "github.com/openfaas/faas-netes/pkg/client/informers/externalversions/openfaas/v1"
	"github.com/openfaas/faas-netes/pkg/config"
	"github.com/openfaas/faas-netes/pkg/controller"
	"github.com/openfaas/faas-netes/pkg/handlers"
	"github.com/openfaas/faas-netes/pkg/k8s"
	"github.com/openfaas/faas-netes/pkg/signals"
	version "github.com/openfaas/faas-netes/version"

	kubeinformers "k8s.io/client-go/informers"
	v1apps "k8s.io/client-go/informers/apps/v1"
	v1core "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"

	// required to authenticate against GKE clusters
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	// required for updating and validating the CRD clientset
	_ "k8s.io/code-generator/cmd/client-gen/generators"
	// main.go:36:2: import "sigs.k8s.io/controller-tools/cmd/controller-gen" is a program, not an importable package
	// _ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)

func main() {
	var kubeconfig string
	var masterURL string
	var flowConfigFile string
	var (
		operator,
		verbose bool
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"Path to a kubeconfig. Only required if out-of-cluster.")
	flag.BoolVar(&verbose, "verbose", false, "Print verbose config information")
	flag.StringVar(&masterURL, "master", "",
		"The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")

	flag.BoolVar(&operator, "operator", false, "Use the operator mode instead of faas-netes")
	flag.StringVar(&flowConfigFile, "flowconfig", "/etc/open-faas/flows/config.json",
		"Path to a flow config file")
	flag.Parse()

	if operator {
		klog.Errorf("The operator mode is deprecated in OpenFaaS Community Edition (CE), upgrade to OpenFaaS Pro to continue using it")
		os.Exit(1)
	}

	mode := "controller"

	sha, release := version.GetReleaseInfo()
	fmt.Printf("faas-netes - Community Edition (CE)\n"+
		"\nVersion: %s Commit: %s Mode: %s\n", release, sha, mode)

	clientCmdConfig, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeconfigQPS := 100
	kubeconfigBurst := 250

	clientCmdConfig.QPS = float32(kubeconfigQPS)
	clientCmdConfig.Burst = kubeconfigBurst

	kubeClient, err := kubernetes.NewForConfig(clientCmdConfig)
	if err != nil {
		log.Fatalf("Error building Kubernetes clientset: %s", err.Error())
	}

	faasClient, err := clientset.NewForConfig(clientCmdConfig)
	if err != nil {
		log.Fatalf("Error building OpenFaaS clientset: %s", err.Error())
	}

	readConfig := config.ReadConfig{}
	osEnv := providertypes.OsEnv{}
	config, err := readConfig.Read(osEnv)

	if err != nil {
		log.Fatalf("Error reading config: %s", err.Error())
	}

	config.Fprint(verbose)

	deployConfig := k8s.DeploymentConfig{
		RuntimeHTTPPort: 8080,
		HTTPProbe:       config.HTTPProbe,
		SetNonRootUser:  config.SetNonRootUser,
		ReadinessProbe: &k8s.ProbeConfig{
			InitialDelaySeconds: int32(config.ReadinessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(config.ReadinessProbeTimeoutSeconds),
			PeriodSeconds:       int32(config.ReadinessProbePeriodSeconds),
		},
		LivenessProbe: &k8s.ProbeConfig{
			InitialDelaySeconds: int32(config.LivenessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(config.LivenessProbeTimeoutSeconds),
			PeriodSeconds:       int32(config.LivenessProbePeriodSeconds),
		},
		ImagePullPolicy:   config.ImagePullPolicy,
		ProfilesNamespace: config.ProfilesNamespace,
	}

	// the sync interval does not affect the scale to/from zero feature
	// auto-scaling is does via the HTTP API that acts on the deployment Spec.Replicas
	defaultResync := time.Minute * 5

	namespaceScope := config.DefaultFunctionNamespace

	if namespaceScope == "" {
		klog.Fatal("DefaultFunctionNamespace must be set")
	}

	kubeInformerOpt := kubeinformers.WithNamespace(namespaceScope)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, defaultResync, kubeInformerOpt)

	faasInformerOpt := informers.WithNamespace(namespaceScope)
	faasInformerFactory := informers.NewSharedInformerFactoryWithOptions(faasClient, defaultResync, faasInformerOpt)

	factory := k8s.NewFunctionFactory(kubeClient, deployConfig, faasClient.OpenfaasV1())

	// Create a Redis client
	isCachingEnable := os.Getenv("IS_CACHING_ENABLE")
	if isCachingEnable == "" {
		isCachingEnable = "false"
	}
	config.FaaSConfig.EnableCaching = isCachingEnable == "true"
	redisClient := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
		DB:   0,
	})

	// Flows config
	var flows providertypes.Flows
	// Read the flow configuration file
	flowsData, err := os.ReadFile(flowConfigFile)
	if err != nil {
		log.Fatalf("Error creating flow configuration: %s", err.Error())
	}
	_ = json.Unmarshal([]byte(flowsData), &flows)

	setup := serverSetup{
		config:              config,
		flows:               flows,
		functionFactory:     factory,
		kubeInformerFactory: kubeInformerFactory,
		faasInformerFactory: faasInformerFactory,
		kubeClient:          kubeClient,
		faasClient:          faasClient,
		redisClient:         redisClient,
	}

	runController(setup)

}

type customInformers struct {
	EndpointsInformer  v1core.EndpointsInformer
	DeploymentInformer v1apps.DeploymentInformer
	FunctionsInformer  v1.FunctionInformer
}

func startInformers(setup serverSetup, stopCh <-chan struct{}, operator bool) customInformers {
	kubeInformerFactory := setup.kubeInformerFactory
	faasInformerFactory := setup.faasInformerFactory

	var functions v1.FunctionInformer
	if operator {
		functions = faasInformerFactory.Openfaas().V1().Functions()
		go functions.Informer().Run(stopCh)
		if ok := cache.WaitForNamedCacheSync("faas-netes:functions", stopCh, functions.Informer().HasSynced); !ok {
			log.Fatalf("failed to wait for cache to sync")
		}
	}

	deployments := kubeInformerFactory.Apps().V1().Deployments()
	go deployments.Informer().Run(stopCh)
	if ok := cache.WaitForNamedCacheSync("faas-netes:deployments", stopCh, deployments.Informer().HasSynced); !ok {
		log.Fatalf("failed to wait for cache to sync")
	}

	endpoints := kubeInformerFactory.Core().V1().Endpoints()
	go endpoints.Informer().Run(stopCh)
	if ok := cache.WaitForNamedCacheSync("faas-netes:endpoints", stopCh, endpoints.Informer().HasSynced); !ok {
		log.Fatalf("failed to wait for cache to sync")
	}

	return customInformers{
		EndpointsInformer:  endpoints,
		DeploymentInformer: deployments,
		FunctionsInformer:  functions,
	}
}

// runController runs the faas-netes imperative controller
func runController(setup serverSetup) {
	config := setup.config
	kubeClient := setup.kubeClient
	factory := setup.functionFactory

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()
	operator := false
	listers := startInformers(setup, stopCh, operator)
	controller.RegisterEventHandlers(listers.DeploymentInformer, kubeClient, config.DefaultFunctionNamespace)

	// TODO: Get the config map of workflow configuration and connections and pass it to the FlowProxy

	functionLookup := k8s.NewFunctionLookup(config.DefaultFunctionNamespace, listers.EndpointsInformer.Lister())
	// TODO: define the flowLookup with a new type here

	bootstrapHandlers := providertypes.FaaSHandlers{
		FunctionProxy: proxy.NewHandlerFunc(config.FaaSConfig, functionLookup),
		// TODO: change the function lookup to the workflow lookup
		Flows:                handlers.MakeFlowsHandler(setup.flows),
		FlowProxy:            proxy.NewFlowHandler(config.FaaSConfig, setup.redisClient, functionLookup, setup.flows),
		DeleteHandler:        handlers.MakeDeleteHandler(config.DefaultFunctionNamespace, kubeClient),
		DeployHandler:        handlers.MakeDeployHandler(config.DefaultFunctionNamespace, factory),
		FunctionReader:       handlers.MakeFunctionReader(config.DefaultFunctionNamespace, listers.DeploymentInformer.Lister()),
		FlowReader:           handlers.MakeFlowReader(config.DefaultFunctionNamespace, nil),
		ReplicaReader:        handlers.MakeReplicaReader(config.DefaultFunctionNamespace, listers.DeploymentInformer.Lister()),
		ReplicaUpdater:       handlers.MakeReplicaUpdater(config.DefaultFunctionNamespace, kubeClient),
		UpdateHandler:        handlers.MakeUpdateHandler(config.DefaultFunctionNamespace, factory),
		HealthHandler:        handlers.MakeHealthHandler(),
		InfoHandler:          handlers.MakeInfoHandler(version.BuildVersion(), version.GitCommit),
		SecretHandler:        handlers.MakeSecretHandler(config.DefaultFunctionNamespace, kubeClient),
		LogHandler:           logs.NewLogHandlerFunc(k8s.NewLogRequestor(kubeClient, config.DefaultFunctionNamespace), config.FaaSConfig.WriteTimeout),
		ListNamespaceHandler: handlers.MakeNamespacesLister(config.DefaultFunctionNamespace, kubeClient),
	}

	faasProvider.Serve(&bootstrapHandlers, &config.FaaSConfig)

}

// serverSetup is a container for the config and clients needed to start the
// faas-netes controller or operator
type serverSetup struct {
	config              config.BootstrapConfig
	flows               providertypes.Flows
	kubeClient          *kubernetes.Clientset
	faasClient          *clientset.Clientset
	redisClient         *redis.Client
	functionFactory     k8s.FunctionFactory
	kubeInformerFactory kubeinformers.SharedInformerFactory
	faasInformerFactory informers.SharedInformerFactory
}
