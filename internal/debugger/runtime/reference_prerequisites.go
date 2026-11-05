package runtime

import (
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/exporter/otlphttpexporter"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"

	"github.com/lineahq/linea/internal/debugger/topology"
)

// CheckReferencePrerequisites rejects known transport prerequisites before any
// reference listeners, sinks or mock providers start. It checks only components
// used by pipelines, using resolved upstream types (including deprecated aliases).
// This is a bounded preflight, not a complete component dependency/isolation audit.
func CheckReferencePrerequisites(cfg *otelcol.Config) error {
	used := usedComponents(cfg)
	less := func(a, b component.ID) int { return strings.Compare(a.String(), b.String()) }
	slices.SortFunc(used.receivers, less)
	slices.SortFunc(used.exporters, less)
	for _, id := range used.receivers {
		c, ok := cfg.Receivers[id].(*otlpreceiver.Config)
		if !ok {
			continue
		}
		for _, protocol := range []struct {
			name string
			tls  bool
		}{
			{"grpc", c.Protocols.GRPC.HasValue() && c.Protocols.GRPC.Get().TLS.HasValue()},
			{"http", c.Protocols.HTTP.HasValue() && c.Protocols.HTTP.Get().ServerConfig.TLS.HasValue()},
		} {
			if protocol.tls {
				return &runresult.ReferencePrerequisiteError{Kind: topology.KindReceiver, Component: id, Setting: "protocols." + protocol.name + ".tls", Reason: "the loopback reference client uses plaintext; configured receiver TLS cannot be reproduced"}
			}
		}
	}
	for _, id := range used.exporters {
		var tls configtls.Config
		var auth bool
		switch c := cfg.Exporters[id].(type) {
		case *otlpexporter.Config:
			tls, auth = c.ClientConfig.TLS.Config, c.ClientConfig.Auth.HasValue()
			// Upstream LoadTLSConfig enables TLS when a CA is supplied,
			// even after the reference override sets insecure: true.
			if tls.CAPem != "" {
				return &runresult.ReferencePrerequisiteError{Kind: topology.KindExporter, Component: id, Setting: "tls.ca_pem", Reason: "a configured CA enables TLS, which the plaintext reference sink cannot accept"}
			}
		case *otlphttpexporter.Config:
			if c.ClientConfig.ProxyURL != "" {
				return &runresult.ReferencePrerequisiteError{Kind: topology.KindExporter, Component: id, Setting: "proxy_url", Reason: "an explicit proxy can redirect the loopback reference delivery outside the local sink"}
			}
			tls, auth = c.ClientConfig.TLS.Config, c.ClientConfig.Auth.HasValue()
		default:
			continue
		}
		if auth {
			// Extensions are inert in every run: no authenticator
			// exists for the real exporter to call.
			return &runresult.ReferencePrerequisiteError{Kind: topology.KindExporter, Component: id, Setting: "auth", Reason: "authenticator extensions do not run, so the reference exporter cannot authenticate"}
		}
		for _, field := range []struct {
			name string
			set  bool
		}{
			{"ca_file", tls.CAFile != ""},
			{"cert_file", tls.CertFile != ""},
			{"key_file", tls.KeyFile != ""},
			{"include_system_ca_certs_pool", tls.IncludeSystemCACertsPool},
			{"tpm", tls.TPMConfig.Enabled},
		} {
			if field.set {
				return &runresult.ReferencePrerequisiteError{Kind: topology.KindExporter, Component: id, Setting: "tls." + field.name, Reason: "TLS files, host trust stores and devices are not captured for reference transports"}
			}
		}
	}
	return nil
}
