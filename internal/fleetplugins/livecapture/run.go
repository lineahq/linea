package livecapture

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
	_ "google.golang.org/grpc/encoding/gzip"
)

func Run(ctx context.Context, config Config, repo Repository) error {
	// Signal cancellation starts drain; monitoring/stop publication keeps its own
	// context until the bounded shutdown has actually retired this authority.
	active, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	s, err := New(active, config, repo)
	if err != nil {
		return err
	}
	defer s.cancel()
	defer s.host.HTTP.CloseIdleConnections()
	controlOptions := []grpc.ServerOption{grpc.MaxRecvMsgSize(1 << 20), grpc.MaxSendMsgSize(64 << 10), grpc.MaxConcurrentStreams(1), grpc.ConnectionTimeout(2 * time.Second)}
	dataOptions := []grpc.ServerOption{grpc.MaxRecvMsgSize(256 << 10), grpc.MaxConcurrentStreams(8), grpc.ConnectionTimeout(2 * time.Second)}
	if !config.TestInsecure {
		cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			return ErrInvalid
		}
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
		controlOptions = append(controlOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
		dataOptions = append(dataOptions, grpc.Creds(credentials.NewTLS(tlsConfig.Clone())))
	}
	control, data := grpc.NewServer(controlOptions...), grpc.NewServer(dataOptions...)
	defer control.Stop()
	defer data.Stop()
	controlpb.RegisterCaptureControlServer(control, captureControl{server: s})
	ptraceotlp.RegisterGRPCServer(data, &traceReceiver{server: s})
	plogotlp.RegisterGRPCServer(data, &logReceiver{server: s})
	pmetricotlp.RegisterGRPCServer(data, &metricReceiver{server: s})
	failures := make(chan error, 3)
	for index, entry := range []struct {
		address string
		server  *grpc.Server
	}{{config.ControlListen, control}, {config.DeliveryListen, data}} {
		listener, err := net.Listen("tcp", entry.address)
		if err != nil {
			return ErrUnavailable
		}
		defer listener.Close()
		go func(server *grpc.Server, l net.Listener, budget *connectionBudget) {
			failures <- server.Serve(&boundedListener{Listener: l, budget: budget})
		}(entry.server, listener, s.budgets[index])
	}
	listener, err := net.Listen("tcp", config.HTTPListen)
	if err != nil {
		return ErrUnavailable
	}
	defer listener.Close()
	httpServer := &http.Server{Handler: s, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	defer httpServer.Close()
	go func() { failures <- httpServer.Serve(&boundedListener{Listener: listener, budget: s.budgets[2]}) }()
	s.Start()
	defer s.Close()
	select {
	case <-ctx.Done():
		s.Drain()
		delay := config.DrainSeconds
		if delay == 0 {
			delay = 2
		}
		timer := time.NewTimer(time.Duration(delay) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.Fatal():
			return ErrUnavailable
		}
		return nil
	case <-s.Fatal():
		return ErrUnavailable
	case <-failures:
		return ErrUnavailable
	}
}
