package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lineahq/linea/internal/fleet/app"
	"github.com/lineahq/linea/internal/fleet/postgres"
)

// version is the release, set at link time with -X main.version=X.Y.Z.
var version = "0.0.0-dev"

func secret(name string) (string, error) {
	value, path := os.Getenv(name), os.Getenv(name+"_FILE")
	if value != "" && path != "" {
		return "", fmt.Errorf("set only %s or its _FILE variant", name)
	}
	if path == "" {
		return value, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s_FILE secret", name)
	}
	if len(data) > 16384 {
		return "", errors.New("secret file exceeds 16 KiB")
	}
	return strings.TrimSpace(string(data)), nil
}
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "probe" {
		fs := flag.NewFlagSet("probe", flag.ContinueOnError)
		fs.SetOutput(stderr)
		endpoint := fs.String("url", "http://127.0.0.1:8080/readyz", "local readiness URL")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get(*endpoint)
		if err != nil {
			return errors.New("fleet readiness probe failed")
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("fleet is not ready")
		}
		return nil
	}
	if len(args) > 0 && (args[0] == "migrate" || args[0] == "bootstrap") {
		command := args[0]
		fs := flag.NewFlagSet(command, flag.ContinueOnError)
		fs.SetOutput(stderr)
		id := fs.String("server-id", "", "stable fleet identity (bootstrap)")
		name := fs.String("name", "initial-admin", "bootstrap credential name")
		runtimeRole := fs.String("runtime-role", "", "existing runtime role to grant narrow DML privileges (migrate)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		database, err := secret("LINEA_FLEET_DATABASE_URL")
		if err != nil {
			return err
		}
		if database == "" {
			return errors.New("database secret required")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if command == "migrate" {
			if err := postgres.Migrate(ctx, database); err != nil {
				return err
			}
			if *runtimeRole != "" {
				if err := postgres.GrantRuntime(ctx, database, *runtimeRole); err != nil {
					return err
				}
			}
			fmt.Fprintln(stdout, "fleet schema is current")
			return nil
		}
		token, err := secret("LINEA_FLEET_BOOTSTRAP_TOKEN")
		if err != nil {
			return err
		}
		if token != "" {
			// The deployment generated the token and keeps it; never print it.
			created, err := postgres.BootstrapToken(ctx, database, *id, *name, token)
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintln(stdout, "fleet admin created")
			} else {
				fmt.Fprintln(stdout, "fleet admin is current")
			}
			return nil
		}
		issued, err := postgres.Bootstrap(ctx, database, *id, *name)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(issued)
	}
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	cfg := app.DefaultConfig()
	fs := flag.NewFlagSet("linea-fleet", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Storage, "storage", cfg.Storage, "postgres (durable, default) or memory (S1 development)")
	fs.DurationVar(&cfg.DrainTimeout, "drain-timeout", cfg.DrainTimeout, "window over which agent sockets are closed after readiness withdrawal (0..30s)")
	fs.StringVar(&cfg.ManagementAddr, "management-listen", cfg.ManagementAddr, "management listener")
	fs.StringVar(&cfg.AgentAddr, "agent-listen", cfg.AgentAddr, "agent listener")
	fs.StringVar(&cfg.PluginsConfig, "plugins-config", "", "operator plugin registration JSON file (optional)")
	fs.StringVar(&cfg.PluginAddr, "plugin-listen", cfg.PluginAddr, "private plugin host listener (only with registrations)")
	fs.StringVar(&cfg.ServerID, "server-id", "", "stable operator-assigned fleet identity (required)")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "TLS certificate for both listeners")
	fs.StringVar(&cfg.TLSKey, "tls-key", "", "TLS private key")
	fs.BoolVar(&cfg.AllowInsecureRemote, "allow-insecure-remote", false, "acknowledge HTTP behind a trusted TLS ingress")
	fs.IntVar(&cfg.Limits.MaxAgents, "max-agents", cfg.Limits.MaxAgents, "inventory capacity (1..100000)")
	fs.IntVar(&cfg.Limits.MaxConnections, "max-connections", cfg.Limits.MaxConnections, "agent TCP connection capacity (1..10000)")
	fs.IntVar(&cfg.Limits.MaxPendingSessions, "max-pending-sessions", cfg.Limits.MaxPendingSessions, "agent handshakes per replica between admission and their first commit (1..10000)")
	fs.Int64Var(&cfg.Limits.MaxMessageBytes, "max-message-bytes", cfg.Limits.MaxMessageBytes, "maximum OpAMP message size (1024..16777216)")
	fs.IntVar(&cfg.Limits.MaxEvents, "max-events", cfg.Limits.MaxEvents, "retained event bound (1..1000000)")
	fs.DurationVar(&cfg.AgentRetention, "agent-retention", cfg.AgentRetention, "delete agents disconnected longer than this (0 keeps them, up to 8760h)")
	attributes := fs.String("agent-attributes", "", "comma-separated attribute keys stored in addition to the built-in allowlist (up to 16)")
	fs.BoolVar(&cfg.StoreHealthErrors, "store-health-errors", false, "store redacted, truncated component health error text")
	fs.BoolVar(&cfg.ConfigManagement, "config-management", false, "enable opt-in remote config management for supervised agents (postgres only)")
	allowedComponents := fs.String("config-allowed-components", "", "comma-separated kind:type components config versions may use (default: any)")
	showVersion := fs.Bool("version", false, "print build version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *attributes != "" {
		cfg.AgentAttributes = strings.Split(*attributes, ",")
	}
	if *allowedComponents != "" {
		cfg.ConfigAllowedComponents = strings.Split(*allowedComponents, ",")
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	var err error
	cfg.DatabaseURL, err = secret("LINEA_FLEET_DATABASE_URL")
	if err != nil {
		return err
	}
	cfg.APIToken, err = secret("LINEA_FLEET_API_TOKEN")
	if err != nil {
		return err
	}
	cfg.AgentToken, err = secret("LINEA_FLEET_AGENT_TOKEN")
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return app.Run(ctx, cfg, version, func(management, agent net.Addr) {
		fmt.Fprintf(stderr, "linea-fleet %s ready: management=%s agent=%s storage=%s\n", version, management, agent, cfg.Storage)
	})
}
func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "linea-fleet: %v\n", err)
		os.Exit(1)
	}
}
