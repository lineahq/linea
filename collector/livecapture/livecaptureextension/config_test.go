package livecaptureextension

import (
	"go.opentelemetry.io/collector/confmap"
	"testing"
)

func TestConfigByteSizesAndSecurityValidation(t *testing.T) {
	c := defaultConfig().(*Config)
	conf := confmap.NewFromStringMap(map[string]any{"limits": map[string]any{"max_queue_bytes": "8MiB", "max_record_bytes": "64KiB"}})
	if err := conf.Unmarshal(c); err != nil {
		t.Fatal(err)
	}
	if c.Limits.MaxQueueBytes != 8<<20 || c.Limits.MaxRecordBytes != 64<<10 {
		t.Fatal("human byte sizes not parsed")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"production-gateway", "gateway-prod", "prod"} {
		c.Environment = "development"
		c.CollectorGroup = group
		c.LocalControl.Enabled = true
		if err := c.Validate(); err == nil {
			t.Fatal("production local control allowed")
		}
	}
	c.CollectorGroup = "development"
	c.LocalControl.Endpoint = "0.0.0.0:17777"
	if err := c.Validate(); err == nil {
		t.Fatal("public local-control bind allowed")
	}
	c.LocalControl.Endpoint = "localhost:17777"
	if err := c.Validate(); err == nil {
		t.Fatal("DNS local-control endpoint allowed")
	}
	c.LocalControl.Enabled = false
	c.Enabled = true
	c.Environment = "production"
	c.Control.Endpoint = "control:443"
	c.Delivery.ClientConfig.Endpoint = "ingress:4317"
	c.Delivery.ClientConfig.TLS.Insecure = true
	if err := c.Validate(); err == nil {
		t.Fatal("production cleartext delivery allowed")
	}
	c.Delivery.ClientConfig.TLS.Insecure = false
	c.Pod = "gateway-0"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Control.TLS.Insecure = true
	if err := c.Validate(); err == nil {
		t.Fatal("production cleartext control allowed")
	}
	c.Control.TLS.Insecure = false
	c.Pod = ""
	if err := c.Validate(); err == nil {
		t.Fatal("remote control without a pod identity allowed")
	}
}
