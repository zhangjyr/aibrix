/*
Copyright 2024 The Aibrix Team.

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
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/vllm-project/aibrix/pkg/cache"
	"github.com/vllm-project/aibrix/pkg/plugins/gateway"
	routing "github.com/vllm-project/aibrix/pkg/plugins/gateway/algorithms"
	"github.com/vllm-project/aibrix/pkg/utils"
	"google.golang.org/grpc/health"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

var (
	grpc_port int
)

func main() {
	flag.IntVar(&grpc_port, "port", 50052, "gRPC port")
	klog.InitFlags(flag.CommandLine)
	defer klog.Flush()
	flag.Parse()

	// Connect to Redis
	redisClient := utils.GetRedisClient()
	defer func() {
		if err := redisClient.Close(); err != nil {
			klog.Warningf("Error closing Redis client: %v", err)
		}
	}()

	stopCh := make(chan struct{})
	defer close(stopCh)
	var config *rest.Config
	var err error

	// ref: https://github.com/kubernetes-sigs/controller-runtime/issues/878#issuecomment-1002204308
	kubeConfig := flag.Lookup("kubeconfig").Value.String()
	if kubeConfig == "" {
		klog.Info("using in-cluster configuration")
		config, err = rest.InClusterConfig()
	} else {
		klog.Infof("using configuration from '%s'", kubeConfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeConfig)
	}

	if err != nil {
		panic(err)
	}

	cache.InitForGateway(config, stopCh, redisClient, routing.NewPackSLORouter)

	// Connect to K8s cluster
	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error creating kubernetes client: %v", err)
	}

	// grpc server init
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", grpc_port))
	if err != nil {
		klog.Fatalf("failed to listen: %v", err)
	}
	gatewayK8sClient, err := versioned.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error on creating gateway k8s client: %v", err)
	}

	s := grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(s, gateway.NewServer(redisClient, k8sClient, gatewayK8sClient))

	healthCheck := health.NewServer()
	healthPb.RegisterHealthServer(s, healthCheck)
	healthCheck.SetServingStatus("gateway-plugin", healthPb.HealthCheckResponse_SERVING)

	klog.Info("starting gRPC server on port :50052")

	go func() {
		if err := http.ListenAndServe("localhost:6060", nil); err != nil {
			klog.Fatalf("failed to setup profiling: %v", err)
		}
	}()

	// shutdown
	var gracefulStop = make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-gracefulStop
		klog.Warningf("signal received: %v, initiating graceful shutdown...", sig)
		s.GracefulStop()
		os.Exit(0)
	}()

	if err := s.Serve(lis); err != nil {
		panic(err)
	}
}
