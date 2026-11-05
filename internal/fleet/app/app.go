// Package app composes the independent fleet server and its listener lifecycle.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lineahq/linea/internal/fleet/api"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/gateway"
	"github.com/lineahq/linea/internal/fleet/inventory"
	"github.com/lineahq/linea/internal/fleet/plugins"
	"github.com/lineahq/linea/internal/fleet/postgres"
	"github.com/lineahq/linea/pluginsdk"
)

// Run returns only after cancellation or a listener failure and bounded shutdown.
// notify runs after both listeners are bound and contains no credentials.
func Run(ctx context.Context, cfg Config, version string, notify func(management, agent net.Addr)) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	return runWithStore(ctx, cfg, version, notify)
}

func runWithStore(ctx context.Context, cfg Config, version string, notify func(management, agent net.Addr)) error {
	registrations, err := plugins.Load(cfg.PluginsConfig, cfg.APIToken, cfg.AgentToken)
	if err != nil {
		return err
	}
	var store fleet.Repository
	var control fleet.ControlRepository
	var credentials *auth.Service
	var apiAccess, agentAccess auth.Authorizer
	var introspector auth.Introspector
	var durable *postgres.Store
	// lost closes when this replica can no longer prove liveness.
	var lost <-chan struct{}
	if cfg.Storage == "postgres" {
		startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		durable, err = postgres.Open(startupCtx, cfg.DatabaseURL, postgres.Options{ServerID: cfg.ServerID, MaxAgents: cfg.Limits.MaxAgents, MaxEvents: cfg.Limits.MaxEvents, Retention: cfg.AgentRetention})
		cancel()
		if err != nil {
			return fmt.Errorf("open fleet database (migrate/bootstrap first): %w", err)
		}
		defer durable.Close()
		store, control, lost = durable, durable, durable.Lost()
		credentials = &auth.Service{Repository: durable}
		apiAccess, agentAccess, introspector = credentials, credentials, credentials
	} else {
		store = fleet.NewStore(cfg.Limits.MaxAgents)
		apiAuth, _ := auth.New(cfg.APIToken)
		agentAuth, _ := auth.New(cfg.AgentToken)
		apiAccess = auth.Static{Bearer: apiAuth, Role: "reader"}
		agentAccess = auth.Static{Bearer: agentAuth, Role: "agent"}
		introspector = auth.Static{Bearer: agentAuth, Role: "agent"}
		cfg.Limits.MaxEvents = 0
	}
	var tlsConfig *tls.Config
	if cfg.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("load TLS identity: %w", err)
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	management, err := net.Listen("tcp", cfg.ManagementAddr)
	if err != nil {
		return fmt.Errorf("management listen: %w", err)
	}
	defer management.Close()
	agent, err := net.Listen("tcp", cfg.AgentAddr)
	if err != nil {
		return fmt.Errorf("agent listen: %w", err)
	}
	defer agent.Close()
	info := api.ServerInfo{ServerID: cfg.ServerID, Version: version, Storage: cfg.Storage, Limits: cfg.Limits}
	if durable != nil {
		info.ReplicaID = durable.ReplicaID()
	}
	apiHandler := api.NewAuthorized(store, apiAccess, info, control, credentials)
	registry := plugins.New(ctx, registrations, store)
	defer registry.Close()
	registry.SetHost(control, introspector)
	if lifecycle, ok := store.(fleet.PluginLifecycleRepository); ok {
		if err := registry.RestoreLifecycle(ctx, lifecycle); err != nil {
			return err
		}
	}
	apiHandler.SetPlugins(registry)
	if cfg.ConfigManagement {
		apiHandler.SetConfigManagement(durable, cfg.ConfigAllowedComponents)
	}
	var host net.Listener
	if len(registrations) > 0 {
		host, err = net.Listen("tcp", cfg.PluginAddr)
		if err != nil {
			return errors.New("plugin host listen failed")
		}
		defer host.Close()
	}
	ingest, err := inventory.New(cfg.AgentAttributes, cfg.StoreHealthErrors)
	if err != nil {
		return err
	}
	gw, err := gateway.NewAuthorized(store, agentAccess, ingest, cfg.Limits.MaxConnections, cfg.Limits.MaxMessageBytes)
	if err != nil {
		return err
	}
	defer gw.Close()
	gw.SetPendingSessions(cfg.Limits.MaxPendingSessions)
	if durable != nil {
		durable.SetSignals(gw.CloseAgent, gw.CloseCredential)
	}
	if cfg.ConfigManagement {
		// Only this opt-in path may send remote config.
		gw.SetConfigManagement(configmgmt.NewReconciler(durable, configmgmt.DefaultBodyCacheBytes), durable)
		durable.SetConfigSignal(gw.ConfigChanged)
	}
	makeServer := func(handler http.Handler) *http.Server {
		var serverTLS *tls.Config
		if tlsConfig != nil {
			// net/http configures ALPN on startup; listeners must not share it.
			serverTLS = tlsConfig.Clone()
		}
		return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout: 30 * time.Second, WriteTimeout: 10 * time.Second,
			MaxHeaderBytes: 16 * 1024, TLSConfig: serverTLS}
	}
	managementServer, agentServer := makeServer(apiHandler), makeServer(gw)
	// Bound all TCP peers, including incomplete TLS/HTTP handshakes, not only
	// agents that have completed their first OpAMP message.
	managementLimit := &limitedListener{Listener: management, slots: make(chan struct{}, 128)}
	agentLimit := &limitedListener{Listener: agent, slots: make(chan struct{}, cfg.Limits.MaxConnections)}
	management, agent = managementLimit, agentLimit
	writers := []func(io.Writer){gw.WriteMetrics, registry.WriteMetrics, func(w io.Writer) {
		fmt.Fprint(w, "# TYPE linea_fleet_tcp_connections gauge\n# TYPE linea_fleet_tcp_refused_total counter\n# TYPE linea_fleet_tcp_connection_limit gauge\n")
		for _, entry := range []struct {
			name string
			l    *limitedListener
		}{{"management", managementLimit}, {"agent", agentLimit}} {
			fmt.Fprintf(w, "linea_fleet_tcp_connections{listener=\"%s\"} %d\nlinea_fleet_tcp_refused_total{listener=\"%s\"} %d\nlinea_fleet_tcp_connection_limit{listener=\"%s\"} %d\n", entry.name, len(entry.l.slots), entry.name, entry.l.refused.Load(), entry.name, cap(entry.l.slots))
		}
	}}
	if database, ok := store.(interface{ WriteMetrics(io.Writer) }); ok {
		writers = append(writers, database.WriteMetrics)
	}
	apiHandler.SetMetrics(writers...)
	errorsCh := make(chan error, 3)
	serve := func(s *http.Server, l net.Listener) {
		var err error
		if tlsConfig != nil {
			err = s.ServeTLS(l, "", "")
		} else {
			err = s.Serve(l)
		}
		errorsCh <- err
	}
	go serve(managementServer, management)
	go serve(agentServer, agent)
	var hostServer *http.Server
	if host != nil {
		host = &limitedListener{Listener: host, slots: make(chan struct{}, 32)}
		hostServer = makeServer(registry)
		hostServer.ReadHeaderTimeout = pluginsdk.RequestTimeout
		hostServer.ReadTimeout = pluginsdk.RequestTimeout
		hostServer.WriteTimeout = 2 * time.Second
		go serve(hostServer, host)
	}
	registry.Start()
	apiHandler.SetReady(true)
	// A replica that lost liveness has already been reaped or soon will be;
	// it must stop serving rather than report on agents it no longer owns.
	healthError := make(chan error, 1)
	stopHealth := make(chan struct{})
	defer close(stopHealth)
	go func() {
		select {
		case <-lost:
			healthError <- fmt.Errorf("%w: replica fenced: %s", fleet.ErrUnavailable, durable.LostReason())
		case <-stopHealth:
		}
	}()
	if notify != nil {
		notify(management.Addr(), agent.Addr())
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-healthError:
	case runErr = <-errorsCh:
		if errors.Is(runErr, http.ErrServerClosed) {
			runErr = nil
		}
	}
	apiHandler.SetReady(false)
	apiHandler.Drain()
	gw.Drain()
	registry.Close()
	if ctx.Err() != nil && runErr == nil && cfg.DrainTimeout > 0 {
		// Close agents gradually so they reconnect to other replicas without a
		// storm. Lost liveness bypasses this and closes authority immediately.
		spreadCtx, stopSpread := context.WithCancel(context.Background())
		spread := make(chan struct{})
		go func() { defer close(spread); gw.Spread(spreadCtx, cfg.DrainTimeout) }()
		select {
		case <-spread:
		case runErr = <-healthError:
		}
		stopSpread()
		<-spread
	}
	gatewayErr := gw.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Stop accepting on both listeners before waiting for outstanding requests.
	_ = management.Close()
	_ = agent.Close()
	var hostErr error
	if host != nil {
		_ = host.Close()
		hostErr = hostServer.Shutdown(shutdownCtx)
		_ = hostServer.Close()
	}
	managementErr := managementServer.Shutdown(shutdownCtx)
	agentErr := agentServer.Shutdown(shutdownCtx)
	_ = managementServer.Close()
	_ = agentServer.Close()
	return errors.Join(runErr, gatewayErr, managementErr, agentErr, hostErr)
}

type limitedListener struct {
	net.Listener
	slots   chan struct{}
	refused atomic.Uint64
}
type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitedConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			l.refused.Add(1)
			_ = c.Close()
		}
	}
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
