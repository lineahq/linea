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
	"google.golang.org/grpc/keepalive"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
	_ "google.golang.org/grpc/encoding/gzip"
)

// gRPC connection bounds. grpc-go's defaults keep a silent connection for two
// hours and an idle or old one forever. Both listeners ping a quiet peer
// every keepaliveTime and close it when the ping goes unanswered, close a
// connection without streams after maxConnectionIdle, and cap request headers.
// Only delivery connections age out: an exporter redials transparently, while
// ending a control connection would end the peer's sessions.
const (
	keepaliveTime     = 30 * time.Second
	keepaliveTimeout  = 10 * time.Second
	maxConnectionIdle = 30 * time.Second
	dataConnectionAge = 5 * time.Minute
	dataAgeGrace      = 5 * time.Second
	maxHeaderList     = 16 << 10
)

func connectionBounds(age time.Duration) []grpc.ServerOption {
	params := keepalive.ServerParameters{Time: keepaliveTime, Timeout: keepaliveTimeout, MaxConnectionIdle: maxConnectionIdle}
	if age > 0 {
		params.MaxConnectionAge, params.MaxConnectionAgeGrace = age, dataAgeGrace
	}
	return []grpc.ServerOption{grpc.KeepaliveParams(params), grpc.MaxHeaderListSize(maxHeaderList), grpc.ConnectionTimeout(2 * time.Second)}
}

// Run serves until ctx ends or a fatal cause arises. Every error it returns
// carries an enum cause (CauseOf); log receives the transition log.
func Run(ctx context.Context, config Config, repo Repository, log *translog.Logger) (result error) {
	// Signal cancellation starts drain; monitoring/stop publication keeps its own
	// context until the bounded shutdown has actually retired this authority.
	active, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	s, err := New(active, config, repo, log)
	if err != nil {
		return err
	}
	log.Log(translog.EventAuthorityAcquired, translog.Authority(repo.AuthorityID()), translog.Instance(s.instance))
	defer func() {
		log.Log(translog.EventAuthorityReleased, translog.Authority(repo.AuthorityID()), translog.Why(CauseOf(result)))
	}()
	defer s.cancel()
	defer s.host.HTTP.CloseIdleConnections()
	defer s.host.SessionHTTP.CloseIdleConnections()
	controlOptions := append(connectionBounds(0), grpc.MaxRecvMsgSize(1<<20), grpc.MaxSendMsgSize(64<<10), grpc.MaxConcurrentStreams(1))
	dataOptions := append(connectionBounds(dataConnectionAge), grpc.MaxRecvMsgSize(256<<10), grpc.MaxConcurrentStreams(8), grpc.InTapHandle(s.preauthenticate))
	if !config.TestInsecure {
		cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
		if err != nil {
			return WithCause(translog.ReasonTLSInvalid, ErrInvalid)
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
			return WithCause(translog.ReasonListenFailed, ErrUnavailable)
		}
		defer listener.Close()
		go func(server *grpc.Server, l net.Listener, budget *connectionBudget) {
			failures <- server.Serve(&boundedListener{Listener: l, budget: budget})
		}(entry.server, listener, s.budgets[index])
	}
	listener, err := net.Listen("tcp", config.HTTPListen)
	if err != nil {
		return WithCause(translog.ReasonListenFailed, ErrUnavailable)
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
		case err := <-s.Fatal():
			return err
		}
		return nil
	case err := <-s.Fatal():
		return err
	case <-failures:
		return WithCause(translog.ReasonListenerStopped, ErrUnavailable)
	}
}
