package livecapture

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/lineahq/linea/pluginsdk"
)

const Version = "0.4.0"

// APIVersion is the plugin-owned management API schema version.
const APIVersion = "2"

// DefaultCaptureLabel names the fleet agent-credential label that lists the
// collector groups a credential may serve captures for.
const DefaultCaptureLabel = "capture"

type Config struct {
	DrainSeconds            int      `json:"drain_seconds,omitempty"`
	ID                      string   `json:"id"`
	Version                 string   `json:"version"`
	HostEndpoint            string   `json:"host_endpoint"`
	AllowInsecureHost       bool     `json:"allow_insecure_host"`
	HostTokenFile           string   `json:"host_token_file"`
	ServiceTokenFile        string   `json:"service_token_file"`
	HTTPListen              string   `json:"http_listen"`
	ControlListen           string   `json:"control_listen"`
	DeliveryListen          string   `json:"delivery_listen"`
	CertFile                string   `json:"tls_cert_file"`
	KeyFile                 string   `json:"tls_key_file"`
	TestInsecure            bool     `json:"test_insecure"`
	CaptureLabel            string   `json:"capture_label,omitempty"`
	RawPrincipals           []string `json:"raw_principals"`
	MaxPeers                int      `json:"max_peers,omitempty"`
	MetricsTokenFile        string   `json:"metrics_token_file,omitempty"`
	hostToken, serviceToken string
	metricsToken            string
}

func Secret(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", ErrInvalid
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return "", ErrInvalid
	}
	return strings.TrimSpace(string(b)), nil
}
func safeName(s string) bool {
	return len(s) > 0 && len(s) <= 128 && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, ErrInvalid
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(b) > 64<<10 {
		return c, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, ErrInvalid
	}
	if !pluginsdk.ValidID(c.ID) || c.Version != Version || c.HTTPListen == "" || c.ControlListen == "" || c.DeliveryListen == "" || c.ControlListen == c.DeliveryListen {
		return c, ErrInvalid
	}
	if c.DrainSeconds < 0 || c.DrainSeconds > 30 {
		return c, ErrInvalid
	}
	if !c.TestInsecure && (c.CertFile == "" || c.KeyFile == "") {
		return c, ErrInvalid
	}
	c.HostEndpoint, err = pluginsdk.Endpoint(c.HostEndpoint, c.AllowInsecureHost)
	if err != nil {
		return c, ErrInvalid
	}
	c.hostToken, err = Secret(c.HostTokenFile)
	if err != nil || !pluginsdk.ValidToken(c.hostToken) {
		return c, ErrInvalid
	}
	c.serviceToken, err = Secret(c.ServiceTokenFile)
	if err != nil || !pluginsdk.ValidToken(c.serviceToken) || c.serviceToken == c.hostToken {
		return c, ErrInvalid
	}
	if c.CaptureLabel == "" {
		c.CaptureLabel = DefaultCaptureLabel
	}
	if !validLabelKey(c.CaptureLabel) || len(c.RawPrincipals) > 64 {
		return c, ErrInvalid
	}
	seen := map[string]bool{c.hostToken: true, c.serviceToken: true}
	if c.MaxPeers == 0 {
		c.MaxPeers = 256
	}
	if c.MaxPeers < 1 || c.MaxPeers > 4096 {
		return c, ErrInvalid
	}
	if c.MetricsTokenFile != "" {
		c.metricsToken, err = Secret(c.MetricsTokenFile)
		if err != nil || !pluginsdk.ValidToken(c.metricsToken) || seen[c.metricsToken] {
			return c, ErrInvalid
		}
		seen[c.metricsToken] = true
	}
	for _, principal := range c.RawPrincipals {
		if !safeName(principal) {
			return c, ErrInvalid
		}
	}
	return c, nil
}

// validLabelKey matches the fleet credential label key syntax.
func validLabelKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-/", c)) {
			return false
		}
	}
	return true
}

func (c Config) captureLabel() string {
	if c.CaptureLabel == "" {
		return DefaultCaptureLabel
	}
	return c.CaptureLabel
}

func (c Config) peerLimit() int {
	if c.MaxPeers == 0 {
		return 256
	}
	return c.MaxPeers
}
