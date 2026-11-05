package app

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/fleet/api"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/inventory"
)

type Config struct {
	DrainTimeout        time.Duration
	Storage             string
	DatabaseURL         string
	ManagementAddr      string
	AgentAddr           string
	PluginAddr          string
	PluginsConfig       string
	ServerID            string
	APIToken            string
	AgentToken          string
	TLSCert             string
	TLSKey              string
	AllowInsecureRemote bool
	Limits              api.Limits
	// AgentRetention deletes agents disconnected for longer; zero keeps them.
	AgentRetention time.Duration
	// AgentAttributes extends the built-in attribute allowlist.
	AgentAttributes []string
	// StoreHealthErrors keeps redacted health error text.
	StoreHealthErrors bool
	// ConfigManagement enables the opt-in config management API and
	// delivery (M6). Reporting-only is the default.
	ConfigManagement bool
	// ConfigAllowedComponents restricts config versions to these
	// kind:type components; empty allows any.
	ConfigAllowedComponents []string
}

func DefaultConfig() Config {
	return Config{Storage: "postgres", ManagementAddr: "127.0.0.1:8080", AgentAddr: "127.0.0.1:4320", PluginAddr: "127.0.0.1:8081",
		DrainTimeout: 20 * time.Second, AgentRetention: 7 * 24 * time.Hour, Limits: api.Limits{MaxAgents: 20000, MaxConnections: 10000, MaxMessageBytes: 1024 * 1024, MaxEvents: 100000, MaxPendingSessions: 400}}
}

func (c Config) Validate() error {
	if c.DrainTimeout < 0 || c.DrainTimeout > 30*time.Second {
		return errors.New("drain-timeout must be within 0..30s")
	}
	if c.AgentRetention < 0 || c.AgentRetention > 365*24*time.Hour {
		return errors.New("agent-retention must be within 0..8760h")
	}
	if _, err := inventory.New(c.AgentAttributes, false); err != nil {
		return fmt.Errorf("agent-attributes: at most %d keys of letters, digits and ._/-", inventory.MaxOperatorKeys)
	}
	if len(c.ServerID) == 0 || len(c.ServerID) > 128 || strings.TrimSpace(c.ServerID) != c.ServerID || strings.ContainsAny(c.ServerID, "\r\n\t") {
		return errors.New("server-id is required (1 to 128 characters)")
	}
	if c.Storage != "memory" && c.Storage != "postgres" {
		return errors.New("storage must be postgres or memory")
	}
	if c.ConfigManagement && c.Storage != "postgres" {
		return errors.New("config-management requires postgres storage")
	}
	if len(c.ConfigAllowedComponents) > 0 && !c.ConfigManagement {
		return errors.New("config-allowed-components requires config-management")
	}
	if len(c.ConfigAllowedComponents) > 256 || !configmgmt.ValidPolicy(c.ConfigAllowedComponents) {
		return errors.New("config-allowed-components: comma-separated kind:type entries (receiver, processor, exporter, connector, extension)")
	}
	if c.Storage == "postgres" {
		if c.DatabaseURL == "" {
			return errors.New("LINEA_FLEET_DATABASE_URL or its _FILE secret is required")
		}
		if c.APIToken != "" || c.AgentToken != "" {
			return errors.New("shared S1 tokens must be unset in PostgreSQL mode; use database credentials")
		}
	} else {
		if _, err := auth.New(c.APIToken); err != nil {
			return fmt.Errorf("LINEA_FLEET_API_TOKEN: %w", err)
		}
		if _, err := auth.New(c.AgentToken); err != nil {
			return fmt.Errorf("LINEA_FLEET_AGENT_TOKEN: %w", err)
		}
		if c.APIToken == c.AgentToken {
			return errors.New("agent and management secrets must be different")
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls-cert and tls-key must be supplied together")
	}
	if c.Limits.MaxAgents < 1 || c.Limits.MaxAgents > 100000 || c.Limits.MaxConnections < 1 || c.Limits.MaxConnections > 10000 || c.Limits.MaxMessageBytes < 1024 || c.Limits.MaxMessageBytes > 16*1024*1024 {
		return errors.New("limits must be within supported ranges: agents 1..100000, connections 1..10000, message bytes 1024..16777216")
	}
	if c.Limits.MaxPendingSessions < 1 || c.Limits.MaxPendingSessions > 10000 {
		return errors.New("max-pending-sessions must be within 1..10000")
	}
	if c.Limits.MaxEvents < 1 || c.Limits.MaxEvents > 1000000 {
		return errors.New("max-events must be within 1..1000000")
	}
	addresses := []string{c.ManagementAddr, c.AgentAddr}
	if c.PluginsConfig != "" {
		addresses = append(addresses, c.PluginAddr)
	}
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("invalid listener address: %w", err)
		}
		ip := net.ParseIP(host)
		if c.TLSCert == "" && !c.AllowInsecureRemote && (ip == nil || !ip.IsLoopback()) {
			return errors.New("plain HTTP listeners must use literal loopback addresses; configure TLS or explicitly allow an insecure remote ingress")
		}
	}
	return nil
}
