/*
Copyright 2023 The K8sGPT Authors.
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

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/k8sgpt-ai/k8sgpt/pkg/server/analyze"
	"github.com/k8sgpt-ai/k8sgpt/pkg/server/config"

	gw2 "buf.build/gen/go/k8sgpt-ai/k8sgpt/grpc-ecosystem/gateway/v2/schema/v1/server_analyzer_service/schemav1gateway"
	gw "buf.build/gen/go/k8sgpt-ai/k8sgpt/grpc-ecosystem/gateway/v2/schema/v1/server_config_service/schemav1gateway"
	rpc "buf.build/gen/go/k8sgpt-ai/k8sgpt/grpc/go/schema/v1/schemav1grpc"
	schemav1 "buf.build/gen/go/k8sgpt-ai/k8sgpt/protocolbuffers/go/schema/v1"
	"github.com/go-logr/zapr"
	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	ctrl "sigs.k8s.io/controller-runtime"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
)

type Config struct {
	Port               string
	MetricsPort        string
	AlertmanagerPort   string
	Backend            string
	Key                string
	Token              string
	Output             string
	ConfigHandler      *config.Handler
	AnalyzeHandler     *analyze.Handler
	Logger             *zap.Logger
	metricsServer      *http.Server
	alertmanagerServer *http.Server
	listener           net.Listener
	EnableHttp         bool
}

type Health struct {
	Status  string `json:"status"`
	Success int    `json:"success"`
	Failure int    `json:"failure"`
}

//nolint:unused
var health = Health{
	Status:  "ok",
	Success: 0,
	Failure: 0,
}

func (s *Config) Shutdown() error {
	return s.listener.Close()
}

// grpcHandlerFunc returns an http.Handler that delegates to grpcServer on incoming gRPC
// connections or otherHandler otherwise.
func grpcHandlerFunc(grpcServer *grpc.Server, otherHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			otherHandler.ServeHTTP(w, r)
		}
	})
}

func (s *Config) Serve() error {
	ctrl.SetLogger(zapr.NewLogger(s.Logger))

	var lis net.Listener
	var err error
	address := fmt.Sprintf(":%s", s.Port)
	lis, err = net.Listen("tcp", address)
	if err != nil {
		return err
	}

	s.ConfigHandler = &config.Handler{}
	s.AnalyzeHandler = &analyze.Handler{}
	s.listener = lis
	s.Logger.Info(fmt.Sprintf("binding api to %s", s.Port))
	grpcServerUnaryInterceptor := grpc.UnaryInterceptor(LogInterceptor(s.Logger))
	grpcServer := grpc.NewServer(grpcServerUnaryInterceptor)
	reflection.Register(grpcServer)
	rpc.RegisterServerConfigServiceServer(grpcServer, s.ConfigHandler)
	rpc.RegisterServerAnalyzerServiceServer(grpcServer, s.AnalyzeHandler)

	if s.EnableHttp {
		s.Logger.Info("enabling rest/http api")
		gwmux := runtime.NewServeMux()
		err = gw.RegisterServerConfigServiceHandlerFromEndpoint(context.Background(), gwmux, fmt.Sprintf("localhost:%s", s.Port),
			[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())})
		if err != nil {
			log.Fatalln("Failed to register gateway:", err)
		}
		err = gw2.RegisterServerAnalyzerServiceHandlerFromEndpoint(context.Background(), gwmux, fmt.Sprintf("localhost:%s", s.Port),
			[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())})
		if err != nil {
			log.Fatalln("Failed to register gateway:", err)
		}

		srv := &http.Server{
			Addr:    address,
			Handler: h2c.NewHandler(grpcHandlerFunc(grpcServer, gwmux), &http2.Server{}),
		}

		if err := srv.Serve(lis); err != nil {
			return err
		}
	} else {
		if err := grpcServer.Serve(
			lis,
		); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	return nil
}

func (s *Config) ServeMetrics() error {
	s.Logger.Info(fmt.Sprintf("binding metrics to %s", s.MetricsPort))
	s.metricsServer = &http.Server{
		ReadHeaderTimeout: 3 * time.Second,
		Addr:              fmt.Sprintf(":%s", s.MetricsPort),
	}
	s.metricsServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/metrics":
			promhttp.Handler().ServeHTTP(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := s.metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Config) ServeAlerts() error {
	s.alertmanagerServer = &http.Server{
		Addr: fmt.Sprintf(":%s", s.AlertmanagerPort),
	}
	s.alertmanagerServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse URL params.
		params := r.URL.Query()

		// Prepare AnalyzeRequest from URL params.
		analyzeRequest := &schemav1.AnalyzeRequest{
			Backend:  params.Get("backend"),
			Language: params.Get("language"),
			Filters:  params["filters"],
			// TODO add later.
			Nocache: true,
			Explain: true,
		}

		// Extract Prometheus Alert payload.
		log.Printf("extracting alert payload")
		var alerts models.PostableAlerts
		decoder := json.NewDecoder(r.Body)
		err := decoder.Decode(&alerts)
		if err != nil {
			w.Write([]byte(fmt.Sprintf("decode alerts. err: %s", err)))
			return
		}

		annotation := params.Get("annotation")

		// For each Alert, analyze events.
		for _, alert := range alerts {
			if namespace, ok := alert.Labels["namespace"]; ok {
				analyzeRequest.Namespace = namespace
			}
			if resp, err := s.AnalyzeHandler.Analyze(r.Context(), analyzeRequest); err != nil {
				w.Write([]byte(fmt.Sprintf("analyze. err: %s", err)))
				return
			} else {
				for _, result := range resp.Results {
					alert.Annotations[annotation] = result.Details
				}
			}
		}

		// TODO Forward alert to original URI.
	})
	if err := s.alertmanagerServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
