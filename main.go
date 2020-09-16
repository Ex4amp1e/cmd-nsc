// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build !windows

// Package main define a nsc application
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/grpcfd"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	kernelmech "github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/kernel"
	vfiomech "github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/vfio"
	"github.com/networkservicemesh/sdk-sriov/pkg/networkservice/common/mechanisms/vfio"
	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/client"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/kernel"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/sendfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/chain"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/jaeger"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/signalctx"
	"github.com/networkservicemesh/sdk/pkg/tools/spanhelper"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"

	"github.com/networkservicemesh/cmd-nsc/pkg/config"
)

func main() {
	// ********************************************************************************
	// Configure signal handling context
	// ********************************************************************************
	ctx := signalctx.WithSignals(context.Background())
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	// ********************************************************************************
	// Setup logger
	// ********************************************************************************
	logrus.Info("Starting NetworkServiceMesh Client ...")
	logrus.SetFormatter(&nested.Formatter{})
	logrus.SetLevel(logrus.TraceLevel)

	ctx = log.WithField(ctx, "cmd", os.Args[:1])

	// ********************************************************************************
	// Configure open tracing
	// ********************************************************************************
	// Enable Jaeger
	jaegerCloser := jaeger.InitJaeger("nsc")
	defer func() { _ = jaegerCloser.Close() }()

	// ********************************************************************************
	// Get config from environment
	// ********************************************************************************
	rootConf := &config.Config{}
	if err := envconfig.Usage("nsm", rootConf); err != nil {
		log.Entry(ctx).Fatal(err)
	}
	if err := envconfig.Process("nsm", rootConf); err != nil {
		log.Entry(ctx).Fatalf("error processing rootConf from env: %+v", err)
	}

	nsmClient := NewNSMClient(ctx, rootConf)
	connections, err := RunClient(ctx, rootConf, nsmClient)
	if err != nil {
		log.Entry(ctx).Errorf("failed to connect to network services: %v", err.Error())
	} else {
		log.Entry(ctx).Infof("All client init operations are done.")
	}

	// Wait for cancel event to terminate
	<-ctx.Done()

	logrus.Infof("Performing cleanup of connections due terminate...")
	for _, c := range connections {
		_, err := nsmClient.Close(context.Background(), c)
		if err != nil {
			logrus.Infof("Failed to close connection %v cause: %v", c, err)
		}
	}
}

// NewNSMClient - creates a client connection to NSMGr
func NewNSMClient(ctx context.Context, rootConf *config.Config) networkservice.NetworkServiceClient {
	// ********************************************************************************
	// Get a x509Source
	// ********************************************************************************
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		log.Entry(ctx).Fatalf("error getting x509 source: %+v", err)
	}
	var svid *x509svid.SVID
	svid, err = source.GetX509SVID()
	if err != nil {
		log.Entry(ctx).Fatalf("error getting x509 svid: %+v", err)
	}
	log.Entry(ctx).Infof("sVID: %q", svid.ID)

	// ********************************************************************************
	// Connect to NSManager
	// ********************************************************************************
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	log.Entry(ctx).Infof("NSC: Connecting to Network Service Manager %v", rootConf.ConnectTo.String())
	var clientCC *grpc.ClientConn
	clientCC, err = grpc.DialContext(ctx,
		grpcutils.URLToTarget(&rootConf.ConnectTo),
		append(spanhelper.WithTracingDial(),
			grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
			grpc.WithTransportCredentials(
				grpcfd.TransportCredentials(
					credentials.NewTLS(
						tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()),
					),
				),
			))...,
	)
	if err != nil {
		log.Entry(ctx).Fatalf("failed to dial NSM: %v", err)
	}

	// ********************************************************************************
	// Create Network Service Manager nsmClient
	// ********************************************************************************
	return client.NewClient(
		ctx,
		rootConf.Name,
		nil,
		spiffejwt.TokenGeneratorFunc(source, rootConf.MaxTokenLifetime),
		clientCC,
		sendfd.NewClient(),
	)
}

// RunClient - runs a client application with passed configuration over a client to Network Service Manager
func RunClient(ctx context.Context, rootConf *config.Config, nsmClient networkservice.NetworkServiceClient) ([]*networkservice.Connection, error) {
	// Validate config parameters
	if err := rootConf.IsValid(); err != nil {
		return []*networkservice.Connection{}, err
	}

	var requestConfigs []*requestConfig
	for i := range rootConf.NetworkServices {
		nsConf := &rootConf.NetworkServices[i]
		err := nsConf.MergeWithConfigOptions(rootConf)
		if err != nil {
			logrus.Errorf("error during nsmClient config aggregation %v", err)
			return []*networkservice.Connection{}, err
		}
		requestConfigs = appendNSConf(nsConf, requestConfigs)
	}

	// ********************************************************************************
	// Initiate connections
	// ********************************************************************************

	// A list of cleanup operations
	var connections []*networkservice.Connection
	for _, requestConfig := range requestConfigs {
		var clients []networkservice.NetworkServiceClient
		for _, nsConf := range requestConfig.nsConfs {
			switch nsConf.Mechanism {
			case kernelmech.MECHANISM:
				clients = append(clients, kernel.NewClient(kernel.WithInterfaceName(nsConf.Path[0])))
			case vfiomech.MECHANISM:
				cgroupDir, err := cgroupDirPath()
				if err != nil {
					log.Entry(ctx).Errorf("failed to get devices cgroup: %v", err)
					return connections, err
				}
				clients = append(clients, vfio.NewClient("/dev/vfio", cgroupDir))
			}
		}
		clients = append(clients, nsmClient)

		// Construct a request
		request := &networkservice.NetworkServiceRequest{
			Connection: &networkservice.Connection{
				Id:             fmt.Sprintf("%s-%d", rootConf.Name, len(connections)),
				NetworkService: requestConfig.nsName,
				Labels:         requestConfig.labels,
			},
		}

		// Performing nsmClient connection request
		conn, connerr := chain.NewNetworkServiceClient(clients...).Request(ctx, request)
		if connerr != nil {
			log.Entry(ctx).Errorf("Failed to request network service with %v: err %v", request, connerr)
			return connections, connerr
		}

		log.Entry(ctx).Infof("Network service established with %v\n Connection:%v", request, conn)
		// Add connection to list
		connections = append(connections, conn)
	}
	return connections, nil
}

type requestConfig struct {
	nsName  string
	labels  map[string]string
	nsConfs []*config.NetworkServiceConfig
}

func appendNSConf(nsConf *config.NetworkServiceConfig, requestConfigs []*requestConfig) []*requestConfig {
	for _, requestConfig := range requestConfigs {
		if requestConfig.nsName == nsConf.NetworkService && reflect.DeepEqual(requestConfig.labels, nsConf.Labels) {
			requestConfig.nsConfs = append(requestConfig.nsConfs, nsConf)
			return requestConfigs
		}
	}
	return append(requestConfigs, &requestConfig{
		nsName:  nsConf.NetworkService,
		labels:  nsConf.Labels,
		nsConfs: []*config.NetworkServiceConfig{nsConf},
	})
}

var devicesCgroup = regexp.MustCompile("^[1-9][0-9]*?:devices:(.*)$")

func cgroupDirPath() (string, error) {
	cgroupInfo, err := os.OpenFile("/proc/self/cgroup", os.O_RDONLY, 0)
	if err != nil {
		return "", errors.Wrap(err, "error opening cgroup info file")
	}
	for scanner := bufio.NewScanner(cgroupInfo); scanner.Scan(); {
		line := scanner.Text()
		if devicesCgroup.MatchString(line) {
			return devicesCgroup.FindStringSubmatch(line)[1], nil
		}
	}
	return "", errors.New("can't find out cgroup directory")
}
