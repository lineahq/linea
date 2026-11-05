package livecaptureextension

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
)

type ByteSize int64

func (b *ByteSize) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	multiplier := int64(1)
	for _, suffix := range []struct {
		name  string
		scale int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1000000000}, {"MB", 1000000}, {"KB", 1000}, {"B", 1}} {
		if strings.HasSuffix(s, suffix.name) {
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix.name))
			multiplier = suffix.scale
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > (1<<63-1)/multiplier {
		return fmt.Errorf("invalid byte size %q", text)
	}
	*b = ByteSize(n * multiplier)
	return nil
}

type Limits struct {
	MaxSessionDuration     time.Duration `mapstructure:"max_session_duration"`
	DefaultSessionDuration time.Duration `mapstructure:"default_session_duration"`
	MaxQueueBytes          ByteSize      `mapstructure:"max_queue_bytes"`
	MaxBytesPerSecond      ByteSize      `mapstructure:"max_bytes_per_second"`
	MaxRecordsPerSecond    int64         `mapstructure:"max_records_per_second"`
	MaxRecordBytes         ByteSize      `mapstructure:"max_record_bytes"`
	MaxTotalBytes          ByteSize      `mapstructure:"max_total_bytes"`
	MaxTotalRecords        int64         `mapstructure:"max_total_records"`
}

type Redaction struct {
	DenyAttributes   []string `mapstructure:"deny_attributes"`
	MaxStringLength  int      `mapstructure:"max_string_length"`
	MaxLogBodyLength int      `mapstructure:"max_log_body_length"`
	Masks            []string `mapstructure:"masks"`
}

type LocalControl struct {
	Enabled  bool   `mapstructure:"enabled"`
	Endpoint string `mapstructure:"endpoint"`
	AllowRaw bool   `mapstructure:"allow_raw"`
}

// Config is Git-owned. No session can change delivery, security, or hard limits.
type Config struct {
	Enabled        bool                    `mapstructure:"enabled"`
	Environment    string                  `mapstructure:"environment"`
	CollectorGroup string                  `mapstructure:"collector_group"`
	Pod            string                  `mapstructure:"pod"`
	Cluster        string                  `mapstructure:"cluster"`
	Namespace      string                  `mapstructure:"namespace"`
	Control        configgrpc.ClientConfig `mapstructure:"control"`
	Delivery       otlpexporter.Config     `mapstructure:"delivery"`
	Limits         Limits                  `mapstructure:"limits"`
	Redaction      Redaction               `mapstructure:"redaction"`
	LocalControl   LocalControl            `mapstructure:"local_control"`
}

func defaultConfig() component.Config {
	return &Config{
		Environment: "production", CollectorGroup: "development",
		Control:  configgrpc.NewDefaultClientConfig(),
		Delivery: *otlpexporter.NewFactory().CreateDefaultConfig().(*otlpexporter.Config),
		Limits: Limits{MaxSessionDuration: 5 * time.Minute, DefaultSessionDuration: time.Minute,
			MaxQueueBytes: 8 << 20, MaxBytesPerSecond: 1 << 20, MaxRecordsPerSecond: 5000,
			MaxRecordBytes: 64 << 10, MaxTotalBytes: 64 << 20, MaxTotalRecords: 100000},
		Redaction:    Redaction{DenyAttributes: []string{"http.request.header.authorization", "db.query.text"}, MaxStringLength: 4096, MaxLogBodyLength: 16384},
		LocalControl: LocalControl{Endpoint: "127.0.0.1:17777"},
	}
}

func (c *Config) Validate() error {
	if c.Environment != "development" && c.Environment != "test" && c.Environment != "production" {
		return errors.New("environment must be development, test, or production")
	}
	if c.CollectorGroup == "" {
		return errors.New("collector_group is required")
	}
	l := c.Limits
	if l.MaxSessionDuration < 5*time.Second || l.MaxSessionDuration > 5*time.Minute || l.DefaultSessionDuration < 5*time.Second || l.DefaultSessionDuration > l.MaxSessionDuration {
		return errors.New("session durations must be between 5s and 5m, with default <= maximum")
	}
	if l.MaxQueueBytes < 3 || l.MaxRecordBytes <= 0 || l.MaxBytesPerSecond <= 0 || l.MaxRecordsPerSecond <= 0 || l.MaxTotalBytes <= 0 || l.MaxTotalRecords <= 0 {
		return errors.New("all queue, rate, record, and session budgets must be positive")
	}
	if c.Redaction.MaxStringLength <= 0 || c.Redaction.MaxLogBodyLength <= 0 {
		return errors.New("redaction length limits must be positive")
	}
	if _, err := newRedactor(c.Redaction); err != nil {
		return err
	}
	production := c.Environment == "production" || strings.Contains(strings.ToLower(c.CollectorGroup), "prod")
	if c.LocalControl.Enabled {
		if production {
			return errors.New("local_control is forbidden for production environments and groups")
		}
		host, _, err := net.SplitHostPort(c.LocalControl.Endpoint)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			return errors.New("local_control.endpoint must use a literal loopback IP and port")
		}
	}
	if c.Enabled {
		if err := c.Delivery.Validate(); err != nil {
			return fmt.Errorf("delivery: %w", err)
		}
		if c.Control.Endpoint == "" && !c.LocalControl.Enabled {
			return errors.New("enabled capture requires control.endpoint or development local_control")
		}
		if production && c.Delivery.ClientConfig.TLS.Insecure {
			return errors.New("production delivery requires TLS")
		}
		if c.Delivery.ClientConfig.TLS.InsecureSkipVerify {
			return errors.New("delivery may not skip TLS verification")
		}
	}
	if c.Control.Endpoint != "" {
		// The service targets and audits sessions per pod.
		if c.Pod == "" {
			return errors.New("pod is required with control.endpoint; set it from the downward API, for example ${env:POD_NAME}")
		}
		if production && c.Control.TLS.Insecure {
			return errors.New("production control requires TLS")
		}
		if c.Control.TLS.InsecureSkipVerify {
			return errors.New("control may not skip TLS verification")
		}
	}
	return nil
}
