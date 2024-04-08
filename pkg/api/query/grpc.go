// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package v1

import (
	"context"
	"time"

	"github.com/prometheus/prometheus/promql"
	"github.com/thanos-io/promql-engine/logicalplan"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/thanos-io/thanos/pkg/api/query/querypb"
	"github.com/thanos-io/thanos/pkg/query"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb/prompb"
)

type GRPCAPI struct {
	now                         func() time.Time
	replicaLabels               []string
	queryableCreate             query.QueryableCreator
	engineFactory               *QueryEngineFactory
	defaultEngine               querypb.EngineType
	lookbackDeltaCreate         func(int64) time.Duration
	defaultMaxResolutionSeconds time.Duration
}

func NewGRPCAPI(
	now func() time.Time,
	replicaLabels []string,
	creator query.QueryableCreator,
	engineFactory *QueryEngineFactory,
	defaultEngine querypb.EngineType,
	lookbackDeltaCreate func(int64) time.Duration,
	defaultMaxResolutionSeconds time.Duration,
) *GRPCAPI {
	return &GRPCAPI{
		now:                         now,
		replicaLabels:               replicaLabels,
		queryableCreate:             creator,
		engineFactory:               engineFactory,
		defaultEngine:               defaultEngine,
		lookbackDeltaCreate:         lookbackDeltaCreate,
		defaultMaxResolutionSeconds: defaultMaxResolutionSeconds,
	}
}

func RegisterQueryServer(queryServer querypb.QueryServer) func(*grpc.Server) {
	return func(s *grpc.Server) {
		querypb.RegisterQueryServer(s, queryServer)
	}
}

func (g *GRPCAPI) Query(request *querypb.QueryRequest, server querypb.Query_QueryServer) error {
	ctx := server.Context()
	var ts time.Time
	if request.TimeSeconds == 0 {
		ts = g.now()
	} else {
		ts = time.Unix(request.TimeSeconds, 0)
	}

	if request.TimeoutSeconds != 0 {
		var cancel context.CancelFunc
		timeout := time.Duration(request.TimeoutSeconds) * time.Second
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	maxResolution := request.MaxResolutionSeconds
	if request.MaxResolutionSeconds == 0 {
		maxResolution = g.defaultMaxResolutionSeconds.Milliseconds() / 1000
	}

	lookbackDelta := g.lookbackDeltaCreate(maxResolution * 1000)
	if request.LookbackDeltaSeconds > 0 {
		lookbackDelta = time.Duration(request.LookbackDeltaSeconds) * time.Second
	}

	storeMatchers, err := querypb.StoreMatchersToLabelMatchers(request.StoreMatchers)
	if err != nil {
		return err
	}

	replicaLabels := g.replicaLabels
	if len(request.ReplicaLabels) != 0 {
		replicaLabels = request.ReplicaLabels
	}

	queryable := g.queryableCreate(
		request.EnableDedup,
		replicaLabels,
		storeMatchers,
		maxResolution,
		request.EnablePartialResponse,
		false,
		request.ShardInfo,
		query.NoopSeriesStatsReporter,
	)

	engineParam := request.Engine
	if engineParam == querypb.EngineType_default {
		engineParam = g.defaultEngine
	}

	var qry promql.Query
	switch engineParam {
	case querypb.EngineType_prometheus:
		queryEngine := g.engineFactory.GetPrometheusEngine()
		qry, err = queryEngine.NewInstantQuery(ctx, queryable, promql.NewPrometheusQueryOpts(false, lookbackDelta), request.Query, ts)
		if err != nil {
			return err
		}
	case querypb.EngineType_thanos:
		queryEngine := g.engineFactory.GetThanosEngine()
		plan, err := logicalplan.Unmarshal(request.QueryPlan.GetJson())
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		qry, err = queryEngine.NewInstantQueryFromPlan(ctx, queryable, promql.NewPrometheusQueryOpts(false, lookbackDelta), plan, ts)
		if err != nil {
			return err
		}
	default:
		return status.Error(codes.InvalidArgument, "invalid engine parameter")
	}
	defer qry.Close()

	result := qry.Exec(ctx)
	if result.Err != nil {
		return status.Error(codes.Aborted, result.Err.Error())
	}

	if len(result.Warnings) != 0 {
		if err := server.Send(querypb.NewQueryWarningsResponse(result.Warnings.AsErrors()...)); err != nil {
			return err
		}
	}

	switch vector := result.Value.(type) {
	case promql.Scalar:
		series := &prompb.TimeSeries{
			Samples: []prompb.Sample{{Value: vector.V, Timestamp: vector.T}},
		}
		if err := server.Send(querypb.NewQueryResponse(series)); err != nil {
			return err
		}
	case promql.Vector:
		for _, sample := range vector {
			floats, histograms := prompb.SamplesFromPromqlSamples(sample)
			series := &prompb.TimeSeries{
				Labels:     labelpb.ZLabelsFromPromLabels(sample.Metric),
				Samples:    floats,
				Histograms: histograms,
			}
			if err := server.Send(querypb.NewQueryResponse(series)); err != nil {
				return err
			}
		}
		return nil
	}

	return nil
}

func (g *GRPCAPI) QueryRange(request *querypb.QueryRangeRequest, srv querypb.Query_QueryRangeServer) error {
	ctx := srv.Context()
	if request.TimeoutSeconds != 0 {
		var cancel context.CancelFunc
		timeout := time.Duration(request.TimeoutSeconds) * time.Second
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	maxResolution := request.MaxResolutionSeconds
	if request.MaxResolutionSeconds == 0 {
		maxResolution = g.defaultMaxResolutionSeconds.Milliseconds() / 1000
	}

	lookbackDelta := g.lookbackDeltaCreate(maxResolution * 1000)
	if request.LookbackDeltaSeconds > 0 {
		lookbackDelta = time.Duration(request.LookbackDeltaSeconds) * time.Second
	}

	storeMatchers, err := querypb.StoreMatchersToLabelMatchers(request.StoreMatchers)
	if err != nil {
		return err
	}

	replicaLabels := g.replicaLabels
	if len(request.ReplicaLabels) != 0 {
		replicaLabels = request.ReplicaLabels
	}

	queryable := g.queryableCreate(
		request.EnableDedup,
		replicaLabels,
		storeMatchers,
		maxResolution,
		request.EnablePartialResponse,
		false,
		request.ShardInfo,
		query.NoopSeriesStatsReporter,
	)

	startTime := time.Unix(request.StartTimeSeconds, 0)
	endTime := time.Unix(request.EndTimeSeconds, 0)
	interval := time.Duration(request.IntervalSeconds) * time.Second

	engineParam := request.Engine
	if engineParam == querypb.EngineType_default {
		engineParam = g.defaultEngine
	}

	var qry promql.Query
	switch engineParam {
	case querypb.EngineType_prometheus:
		queryEngine := g.engineFactory.GetPrometheusEngine()
		qry, err = queryEngine.NewRangeQuery(ctx, queryable, promql.NewPrometheusQueryOpts(false, lookbackDelta), request.Query, startTime, endTime, interval)
		if err != nil {
			return err
		}
	case querypb.EngineType_thanos:
		thanosEngine := g.engineFactory.GetThanosEngine()
		plan, err := logicalplan.Unmarshal(request.QueryPlan.GetJson())
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		qry, err = thanosEngine.NewRangeQueryFromPlan(ctx, queryable, promql.NewPrometheusQueryOpts(false, lookbackDelta), plan, startTime, endTime, interval)
		if err != nil {
			return err
		}
	default:
		return status.Error(codes.InvalidArgument, "invalid engine parameter")
	}
	defer qry.Close()

	result := qry.Exec(ctx)
	if result.Err != nil {
		return status.Error(codes.Aborted, result.Err.Error())
	}

	if len(result.Warnings) != 0 {
		if err := srv.Send(querypb.NewQueryRangeWarningsResponse(result.Warnings.AsErrors()...)); err != nil {
			return err
		}
	}

	switch value := result.Value.(type) {
	case promql.Matrix:
		for _, series := range value {
			floats, histograms := prompb.SamplesFromPromqlSeries(series)
			series := &prompb.TimeSeries{
				Labels:     labelpb.ZLabelsFromPromLabels(series.Metric),
				Samples:    floats,
				Histograms: histograms,
			}
			if err := srv.Send(querypb.NewQueryRangeResponse(series)); err != nil {
				return err
			}
		}
	case promql.Vector:
		for _, sample := range value {
			floats, histograms := prompb.SamplesFromPromqlSamples(sample)
			series := &prompb.TimeSeries{
				Labels:     labelpb.ZLabelsFromPromLabels(sample.Metric),
				Samples:    floats,
				Histograms: histograms,
			}
			if err := srv.Send(querypb.NewQueryRangeResponse(series)); err != nil {
				return err
			}
		}
		return nil
	case promql.Scalar:
		series := &prompb.TimeSeries{
			Samples: []prompb.Sample{{Value: value.V, Timestamp: value.T}},
		}
		return srv.Send(querypb.NewQueryRangeResponse(series))
	}

	return nil
}
