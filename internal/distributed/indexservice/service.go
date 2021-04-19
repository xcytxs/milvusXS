// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package grpcindexservice

import (
	"context"
	"io"
	"math"
	"net"
	"strconv"
	"sync"

	"go.uber.org/zap"

	otgrpc "github.com/opentracing-contrib/go-grpc"
	"github.com/opentracing/opentracing-go"
	"github.com/zilliztech/milvus-distributed/internal/indexservice"
	"github.com/zilliztech/milvus-distributed/internal/log"
	"github.com/zilliztech/milvus-distributed/internal/proto/commonpb"
	"github.com/zilliztech/milvus-distributed/internal/proto/indexpb"
	"github.com/zilliztech/milvus-distributed/internal/proto/internalpb"
	"github.com/zilliztech/milvus-distributed/internal/proto/milvuspb"
	"github.com/zilliztech/milvus-distributed/internal/util/funcutil"
	"github.com/zilliztech/milvus-distributed/internal/util/trace"
	"github.com/zilliztech/milvus-distributed/internal/util/typeutil"
	"google.golang.org/grpc"
)

type UniqueID = typeutil.UniqueID
type Timestamp = typeutil.Timestamp

type Server struct {
	indexservice *indexservice.IndexService

	grpcServer  *grpc.Server
	grpcErrChan chan error

	loopCtx    context.Context
	loopCancel func()
	loopWg     sync.WaitGroup

	closer io.Closer
}

func (s *Server) Run() error {

	if err := s.init(); err != nil {
		return err
	}

	if err := s.start(); err != nil {
		return err
	}
	return nil
}

func (s *Server) init() error {
	Params.Init()
	indexservice.Params.Init()

	closer := trace.InitTracing("index_service")
	s.closer = closer

	s.loopWg.Add(1)
	go s.startGrpcLoop(Params.ServicePort)
	// wait for grpc indexservice loop start
	if err := <-s.grpcErrChan; err != nil {
		return err
	}
	s.indexservice.UpdateStateCode(internalpb.StateCode_Initializing)

	if err := s.indexservice.Init(); err != nil {
		return err
	}
	return nil
}

func (s *Server) start() error {
	if err := s.indexservice.Start(); err != nil {
		return err
	}
	log.Debug("indexService started")
	return nil
}

func (s *Server) Stop() error {
	if s.closer != nil {
		if err := s.closer.Close(); err != nil {
			return err
		}
	}
	if s.indexservice != nil {
		s.indexservice.Stop()
	}

	s.loopCancel()
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}

	s.loopWg.Wait()
	return nil
}

func (s *Server) GetComponentStates(ctx context.Context, req *internalpb.GetComponentStatesRequest) (*internalpb.ComponentStates, error) {
	return s.indexservice.GetComponentStates(ctx)
}

func (s *Server) GetTimeTickChannel(ctx context.Context, req *internalpb.GetTimeTickChannelRequest) (*milvuspb.StringResponse, error) {
	return s.indexservice.GetTimeTickChannel(ctx)
}

func (s *Server) GetStatisticsChannel(ctx context.Context, req *internalpb.GetStatisticsChannelRequest) (*milvuspb.StringResponse, error) {
	return s.indexservice.GetStatisticsChannel(ctx)
}

func (s *Server) RegisterNode(ctx context.Context, req *indexpb.RegisterNodeRequest) (*indexpb.RegisterNodeResponse, error) {
	return s.indexservice.RegisterNode(ctx, req)
}

func (s *Server) BuildIndex(ctx context.Context, req *indexpb.BuildIndexRequest) (*indexpb.BuildIndexResponse, error) {
	return s.indexservice.BuildIndex(ctx, req)
}

func (s *Server) GetIndexStates(ctx context.Context, req *indexpb.GetIndexStatesRequest) (*indexpb.GetIndexStatesResponse, error) {
	return s.indexservice.GetIndexStates(ctx, req)
}

func (s *Server) DropIndex(ctx context.Context, request *indexpb.DropIndexRequest) (*commonpb.Status, error) {
	return s.indexservice.DropIndex(ctx, request)
}

func (s *Server) GetIndexFilePaths(ctx context.Context, req *indexpb.GetIndexFilePathsRequest) (*indexpb.GetIndexFilePathsResponse, error) {
	return s.indexservice.GetIndexFilePaths(ctx, req)
}

func (s *Server) NotifyBuildIndex(ctx context.Context, nty *indexpb.NotifyBuildIndexRequest) (*commonpb.Status, error) {
	return s.indexservice.NotifyBuildIndex(ctx, nty)
}
func (s *Server) startGrpcLoop(grpcPort int) {

	defer s.loopWg.Done()

	log.Debug("indexservice", zap.Int("network port", grpcPort))
	lis, err := net.Listen("tcp", ":"+strconv.Itoa(grpcPort))
	if err != nil {
		log.Warn("indexservice", zap.String("GrpcServer:failed to listen", err.Error()))
		s.grpcErrChan <- err
		return
	}

	ctx, cancel := context.WithCancel(s.loopCtx)
	defer cancel()

	tracer := opentracing.GlobalTracer()
	s.grpcServer = grpc.NewServer(
		grpc.MaxRecvMsgSize(math.MaxInt32),
		grpc.MaxSendMsgSize(math.MaxInt32),
		grpc.UnaryInterceptor(
			otgrpc.OpenTracingServerInterceptor(tracer)),
		grpc.StreamInterceptor(
			otgrpc.OpenTracingStreamServerInterceptor(tracer)))
	indexpb.RegisterIndexServiceServer(s.grpcServer, s)

	go funcutil.CheckGrpcReady(ctx, s.grpcErrChan)
	if err := s.grpcServer.Serve(lis); err != nil {
		s.grpcErrChan <- err
	}
}

func NewServer(ctx context.Context) (*Server, error) {

	ctx1, cancel := context.WithCancel(ctx)
	serverImp, err := indexservice.NewIndexService(ctx)
	if err != nil {
		defer cancel()
		return nil, err
	}
	s := &Server{
		loopCtx:      ctx1,
		loopCancel:   cancel,
		indexservice: serverImp,
		grpcErrChan:  make(chan error),
	}

	return s, nil
}
