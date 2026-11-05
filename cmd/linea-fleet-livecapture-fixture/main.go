// A disposable synthetic OTLP input generator, never part of the server image.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/lineahq/linea/pluginsdk"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	endpoint := flag.String("endpoint", "collector:4317", "synthetic fixture OTLP/gRPC receiver")
	caFile := flag.String("tls-ca", "", "capture ingress CA for refusal checks")
	serverName := flag.String("server-name", "", "explicit TLS peer name for refusal checks")
	tokenFile := flag.String("token-file", "", "fleet agent credential for refusal checks")
	expectCode := flag.String("expect-code", "", "expected gRPC refusal code; empty sends ordinary input")
	oversize := flag.Bool("oversize", false, "send one synthetic request over the ingress bound")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("invalid fixture arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := insecure.NewCredentials()
	if *caFile != "" {
		tlsTransport, err := credentials.NewClientTLSFromFile(*caFile, *serverName)
		if err != nil {
			return errors.New("fixture TLS setup failed")
		}
		transport = tlsTransport
	}
	conn, err := grpc.NewClient(*endpoint, grpc.WithTransportCredentials(transport))
	if err != nil {
		return errors.New("fixture input connection failed")
	}
	defer conn.Close()
	if *tokenFile != "" {
		secret, err := os.ReadFile(*tokenFile)
		if err != nil {
			return errors.New("fixture credential unavailable")
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.TrimSpace(string(secret)))
	}
	attrs := func(r pcommon.Resource) {
		r.Attributes().PutStr("service.name", "s4-fixture")
		r.Attributes().PutStr("http.request.header.authorization", "Bearer synthetic-secret")
	}
	t := ptrace.NewTraces()
	tr := t.ResourceSpans().AppendEmpty()
	attrs(tr.Resource())
	if *expectCode != "" {
		for key, value := range map[string]string{"capture.session.id": pluginsdk.RandomID(), "capture.point": "egress", "capture.pipeline": "traces", "capture.collector.pod": "forged", "capture.cluster": "s4"} {
			tr.Resource().Attributes().PutStr(key, value)
		}
	}
	if *oversize {
		tr.Resource().Attributes().PutStr("synthetic", strings.Repeat("x", 300<<10))
	}
	spans := tr.ScopeSpans().AppendEmpty().Spans()
	for i := 0; i < 2; i++ {
		record := spans.AppendEmpty()
		record.SetName("synthetic")
		record.SetTraceID(pcommon.TraceID{1})
		record.SetSpanID(pcommon.SpanID{byte(i + 1)})
		if i == 0 {
			record.Status().SetCode(ptrace.StatusCodeError)
		}
	}
	_, traceErr := ptraceotlp.NewGRPCClient(conn).Export(ctx, ptraceotlp.NewExportRequestFromTraces(t))
	if *expectCode != "" {
		if status.Code(traceErr).String() != *expectCode {
			return errors.New("capture ingress refusal differed from expected code")
		}
		fmt.Println("expected capture ingress refusal observed")
		return nil
	}
	if traceErr != nil {
		return errors.New("fixture trace input failed")
	}
	l := plog.NewLogs()
	lr := l.ResourceLogs().AppendEmpty()
	attrs(lr.Resource())
	logs := lr.ScopeLogs().AppendEmpty().LogRecords()
	for i := 0; i < 2; i++ {
		record := logs.AppendEmpty()
		record.Body().SetStr("synthetic")
		if i == 0 {
			record.SetSeverityNumber(plog.SeverityNumberError)
		} else {
			record.SetSeverityNumber(plog.SeverityNumberInfo)
		}
	}
	if _, err := plogotlp.NewGRPCClient(conn).Export(ctx, plogotlp.NewExportRequestFromLogs(l)); err != nil {
		return errors.New("fixture log input failed")
	}
	m := pmetric.NewMetrics()
	mr := m.ResourceMetrics().AppendEmpty()
	attrs(mr.Resource())
	metric := mr.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	metric.SetName("synthetic")
	points := metric.SetEmptyGauge().DataPoints()
	for i := 0; i < 2; i++ {
		point := points.AppendEmpty()
		point.SetIntValue(int64(i + 1))
		point.Attributes().PutBool("keep", i == 0)
	}
	if _, err := pmetricotlp.NewGRPCClient(conn).Export(ctx, pmetricotlp.NewExportRequestFromMetrics(m)); err != nil {
		return errors.New("fixture metric input failed")
	}
	fmt.Println("synthetic traces/logs/metrics sent")
	return nil
}
