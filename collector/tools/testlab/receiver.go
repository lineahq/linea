package main

import (
	"context"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

func metadata(resource pcommon.Resource, signal string) (entry, bool) {
	get := func(key string) string {
		value, ok := resource.Attributes().Get(key)
		if !ok || value.Type() != pcommon.ValueTypeStr {
			return ""
		}
		return value.Str()
	}
	item := entry{Signal: signal, SessionID: get("capture.session.id"), Point: get("capture.point"), Pipeline: get("capture.pipeline"), Service: get("service.name"), Records: 1}
	return item, item.SessionID != "" && item.Point != "" && item.Pipeline != ""
}

type traceReceiver struct {
	ptraceotlp.UnimplementedGRPCServer
	store *captureStore
}

func (s *traceReceiver) Export(_ context.Context, request ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	var rejected int64
	for _, resource := range request.Traces().ResourceSpans().All() {
		base, valid := metadata(resource.Resource(), "traces")
		for _, scope := range resource.ScopeSpans().All() {
			for _, span := range scope.Spans().All() {
				if !valid {
					rejected++
					s.store.reject(1)
					continue
				}
				copy := ptrace.NewTraces()
				r := copy.ResourceSpans().AppendEmpty()
				resource.Resource().CopyTo(r.Resource())
				r.SetSchemaUrl(resource.SchemaUrl())
				sc := r.ScopeSpans().AppendEmpty()
				scope.Scope().CopyTo(sc.Scope())
				sc.SetSchemaUrl(scope.SchemaUrl())
				span.CopyTo(sc.Spans().AppendEmpty())
				item := base
				item.Summary = span.Name()
				encoded, err := ptraceotlp.NewExportRequestFromTraces(copy).MarshalJSON()
				if err != nil {
					rejected++
					s.store.reject(1)
					continue
				}
				item.OTLP = encoded
				if !s.store.add(item) {
					rejected++
				}
			}
		}
	}
	response := ptraceotlp.NewExportResponse()
	if rejected > 0 {
		response.PartialSuccess().SetRejectedSpans(rejected)
		response.PartialSuccess().SetErrorMessage("test viewer accepts tagged capture copies within its memory limits only")
	}
	return response, nil
}

type logReceiver struct {
	plogotlp.UnimplementedGRPCServer
	store *captureStore
}

func (s *logReceiver) Export(_ context.Context, request plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	var rejected int64
	for _, resource := range request.Logs().ResourceLogs().All() {
		base, valid := metadata(resource.Resource(), "logs")
		for _, scope := range resource.ScopeLogs().All() {
			for _, record := range scope.LogRecords().All() {
				if !valid {
					rejected++
					s.store.reject(1)
					continue
				}
				copy := plog.NewLogs()
				r := copy.ResourceLogs().AppendEmpty()
				resource.Resource().CopyTo(r.Resource())
				r.SetSchemaUrl(resource.SchemaUrl())
				sc := r.ScopeLogs().AppendEmpty()
				scope.Scope().CopyTo(sc.Scope())
				sc.SetSchemaUrl(scope.SchemaUrl())
				record.CopyTo(sc.LogRecords().AppendEmpty())
				item := base
				item.Summary = record.SeverityText() + " " + record.Body().AsString()
				encoded, err := plogotlp.NewExportRequestFromLogs(copy).MarshalJSON()
				if err != nil {
					rejected++
					s.store.reject(1)
					continue
				}
				item.OTLP = encoded
				if !s.store.add(item) {
					rejected++
				}
			}
		}
	}
	response := plogotlp.NewExportResponse()
	if rejected > 0 {
		response.PartialSuccess().SetRejectedLogRecords(rejected)
		response.PartialSuccess().SetErrorMessage("test viewer accepts tagged capture copies within its memory limits only")
	}
	return response, nil
}

type metricReceiver struct {
	pmetricotlp.UnimplementedGRPCServer
	store *captureStore
}

func (s *metricReceiver) Export(_ context.Context, request pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	var rejected int64
	for _, resource := range request.Metrics().ResourceMetrics().All() {
		base, valid := metadata(resource.Resource(), "metrics")
		for _, scope := range resource.ScopeMetrics().All() {
			for _, metric := range scope.Metrics().All() {
				copy := pmetric.NewMetrics()
				r := copy.ResourceMetrics().AppendEmpty()
				resource.Resource().CopyTo(r.Resource())
				r.SetSchemaUrl(resource.SchemaUrl())
				sc := r.ScopeMetrics().AppendEmpty()
				scope.Scope().CopyTo(sc.Scope())
				sc.SetSchemaUrl(scope.SchemaUrl())
				metric.CopyTo(sc.Metrics().AppendEmpty())
				count := copy.DataPointCount()
				if !valid {
					rejected += int64(count)
					s.store.reject(count)
					continue
				}
				item := base
				item.Records = count
				item.Summary = strings.TrimSpace(metric.Name() + " " + metric.Unit())
				encoded, err := pmetricotlp.NewExportRequestFromMetrics(copy).MarshalJSON()
				if err != nil {
					rejected += int64(count)
					s.store.reject(count)
					continue
				}
				item.OTLP = encoded
				if !s.store.add(item) {
					rejected += int64(count)
				}
			}
		}
	}
	response := pmetricotlp.NewExportResponse()
	if rejected > 0 {
		response.PartialSuccess().SetRejectedDataPoints(rejected)
		response.PartialSuccess().SetErrorMessage("test viewer accepts tagged capture copies within its memory limits only")
	}
	return response, nil
}
