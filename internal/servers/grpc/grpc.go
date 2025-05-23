// Copyright 2018 The Cloudprober Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package grpc provides a simple gRPC server that acts as a probe target.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"

	configpb "github.com/cloudprober/cloudprober/internal/servers/grpc/proto"
	pb "github.com/cloudprober/cloudprober/internal/servers/grpc/proto"
	spb "github.com/cloudprober/cloudprober/internal/servers/grpc/proto"
	"github.com/cloudprober/cloudprober/logger"
	"github.com/cloudprober/cloudprober/metrics"
	"github.com/cloudprober/cloudprober/probes/probeutils"
	"github.com/cloudprober/cloudprober/state"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Server implements a gRPCServer.
type Server struct {
	c            *configpb.ServerConf
	ln           net.Listener
	grpcSrv      *grpc.Server
	healthSrv    *health.Server
	l            *logger.Logger
	startTime    time.Time
	dedicatedSrv bool
	msg          []byte

	// Required for all gRPC server implementations.
	spb.UnimplementedProberServer
}

var (
	maxMsgSize = 1 * 1024 * 1024 // 1MB
	msgPattern = []byte("cloudprober")
)

// Echo reflects back the incoming message.
// TODO: return error if EchoMessage is greater than maxMsgSize.
func (s *Server) Echo(ctx context.Context, req *pb.EchoMessage) (*pb.EchoMessage, error) {
	return req, nil
}

// BlobRead returns a blob of data.
func (s *Server) BlobRead(ctx context.Context, req *pb.BlobReadRequest) (*pb.BlobReadResponse, error) {
	reqSize := req.GetSize()
	if reqSize > int32(maxMsgSize) {
		return nil, fmt.Errorf("read request size (%d) exceeds max size (%d)", reqSize, maxMsgSize)
	}
	return &pb.BlobReadResponse{
		Blob: s.msg[0:reqSize],
	}, nil
}

// ServerStatus returns the current server status.
func (s *Server) ServerStatus(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	return &pb.StatusResponse{
		UptimeUs: proto.Int64(time.Since(s.startTime).Nanoseconds() / 1000),
	}, nil
}

// BlobWrite returns the size of blob in the WriteRequest. It does not operate
// on the blob.
func (s *Server) BlobWrite(ctx context.Context, req *pb.BlobWriteRequest) (*pb.BlobWriteResponse, error) {
	reqSize := int32(len(req.Blob))
	if reqSize > int32(maxMsgSize) {
		return nil, fmt.Errorf("write request size (%d) exceeds max size (%d)", reqSize, maxMsgSize)
	}
	return &pb.BlobWriteResponse{
		Size: proto.Int32(reqSize),
	}, nil
}

// New returns a Server.
func New(initCtx context.Context, c *configpb.ServerConf, l *logger.Logger) (*Server, error) {
	srv := &Server{
		c: c,
		l: l,
	}
	srv.msg = make([]byte, maxMsgSize)
	probeutils.PatternPayload(srv.msg, msgPattern)
	if c.GetUseDedicatedServer() {
		if err := srv.newGRPCServer(initCtx); err != nil {
			return nil, err
		}
		srv.dedicatedSrv = true
		return srv, nil
	}

	defGRPCSrv := state.DefaultGRPCServer()
	if defGRPCSrv == nil {
		return nil, errors.New("initialization of gRPC server failed as default gRPC server is not configured")
	}
	l.Warningf("Reusing global gRPC server %v to handle gRPC probes", defGRPCSrv)
	srv.grpcSrv = defGRPCSrv
	srv.dedicatedSrv = false
	srv.startTime = time.Now()
	spb.RegisterProberServer(defGRPCSrv, srv)
	return srv, nil
}

func (s *Server) newGRPCServer(ctx context.Context) error {
	grpcSrv := grpc.NewServer()
	healthSrv := health.NewServer()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.c.GetPort()))
	if err != nil {
		return err
	}
	// Cleanup listener if ctx is canceled.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	s.ln = ln
	s.grpcSrv = grpcSrv
	s.healthSrv = healthSrv
	s.startTime = time.Now()

	spb.RegisterProberServer(grpcSrv, s)
	healthpb.RegisterHealthServer(grpcSrv, healthSrv)
	return nil
}

// Start starts the gRPC server and serves requests until the context is
// canceled or the gRPC server panics.
func (s *Server) Start(ctx context.Context, dataChan chan<- *metrics.EventMetrics) error {
	if !s.dedicatedSrv {
		// Nothing to do as caller owns server. Wait till context is done.
		<-ctx.Done()
		return nil
	}

	s.l.Infof("Starting gRPC server at %s", s.ln.Addr().String())
	go func() {
		<-ctx.Done()
		s.l.Infof("Context canceled. Shutting down the gRPC server at: %s", s.ln.Addr().String())
		for svc := range s.grpcSrv.GetServiceInfo() {
			s.healthSrv.SetServingStatus(svc, healthpb.HealthCheckResponse_NOT_SERVING)
		}
		s.grpcSrv.Stop()
	}()
	for si := range s.grpcSrv.GetServiceInfo() {
		s.healthSrv.SetServingStatus(si, healthpb.HealthCheckResponse_SERVING)
	}
	if s.c.GetEnableReflection() {
		s.l.Infof("Enabling reflection for gRPC server at %s", s.ln.Addr().String())
		reflection.Register(s.grpcSrv)
	}
	return s.grpcSrv.Serve(s.ln)
}
