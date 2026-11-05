// Testlab is a local, in-memory test fixture for Collector capture development.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip"
)

func main() {
	check := flag.Bool("check", false, "exercise the running lab's capture lifecycle")
	perf := flag.Bool("perf", false, "run isolated Collector load/failure experiments (Linux)")
	perfBinary := flag.String("perf-collector", "/otelcol-livecapture", "Collector binary for experiments")
	perfSeconds := flag.Int("perf-seconds", 8, "load duration per experiment, seconds")
	perfCases := flag.String("perf-cases", "", "comma-separated experiment names (empty runs all)")
	flag.Parse()
	if *perf {
		if err := runPerformance(*perfBinary, time.Duration(*perfSeconds)*time.Second, *perfCases); err != nil {
			log.Fatal(err)
		}
		return
	}
	if *check {
		if err := checkLab("http://127.0.0.1:8080"); err != nil {
			log.Fatal(err)
		}
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, err := grpc.NewClient("127.0.0.1:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:14317")
	if err != nil {
		return err
	}
	store := &captureStore{}
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(2 << 20))
	ptraceotlp.RegisterGRPCServer(grpcServer, &traceReceiver{store: store})
	plogotlp.RegisterGRPCServer(grpcServer, &logReceiver{store: store})
	pmetricotlp.RegisterGRPCServer(grpcServer, &metricReceiver{store: store})
	generator := &generator{conn: conn, state: generatorState{Enabled: true, Rate: 5}}
	client := &http.Client{Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()
	lab := &labServer{store: store, generator: generator, controlURL: "http://127.0.0.1:17777", client: client}
	server := &http.Server{Addr: "0.0.0.0:8080", Handler: lab.handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	var workers sync.WaitGroup
	workers.Add(3)
	failures := make(chan error, 2)
	go func() {
		defer workers.Done()
		if err := grpcServer.Serve(listener); err != nil {
			failures <- err
		}
	}()
	go func() {
		defer workers.Done()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			failures <- err
		}
	}()
	go func() { defer workers.Done(); generator.run(ctx) }()
	log.Println("Local capture test lab ready on http://localhost:8080; generator enabled, capture initially off")
	var result error
	select {
	case <-ctx.Done():
	case err := <-failures:
		result = fmt.Errorf("test lab server: %w", err)
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 4*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	grpcServer.Stop()
	workers.Wait()
	return result
}
