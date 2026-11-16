// Package plugins owns operator registrations and the plugin host boundary.
package plugins

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lineahq/linea/pluginsdk"
)

type Config struct {
	ID                string   `json:"id"`
	Version           string   `json:"version"`
	Endpoint          string   `json:"endpoint"`
	Enabled           bool     `json:"enabled"`
	Permissions       []string `json:"permissions"`
	HostTokenFile     string   `json:"host_token_file"`
	ServiceTokenFile  string   `json:"service_token_file"`
	AllowInsecureHTTP bool     `json:"allow_insecure_http"`
	APIVersion        string   `json:"api_version,omitempty"`
	// CAFile is a PEM bundle trusted for an https endpoint instead of the
	// system roots, for a plugin certificate from a private CA.
	CAFile       string `json:"ca_file,omitempty"`
	hostToken    string
	serviceToken string
	roots        *x509.CertPool
}

// trustRoots makes an HTTP client trust only the configured CA bundle; it
// changes nothing without one.
func (c Config) trustRoots(client *http.Client) {
	if c.roots == nil {
		return
	}
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12}
}

// loadRoots reads a PEM bundle of at most 64 KiB with at least one
// certificate.
func loadRoots(path string) (*x509.CertPool, error) {
	b, err := readLimited(path, 64<<10)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errConfig
	}
	return pool, nil
}

var errConfig = errors.New("invalid plugin configuration or secret files")

// knownPermissions gates the generic host services a registration may use.
var knownPermissions = []string{pluginsdk.AgentsRead, pluginsdk.CredentialsIntrospect}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errConfig
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errConfig
	}
	return b, nil
}

func Load(path string, reservedTokens ...string) ([]Config, error) {
	if path == "" {
		return nil, nil
	}
	b, err := readLimited(path, 64<<10)
	if err != nil {
		return nil, err
	}
	var file struct {
		Plugins []Config `json:"plugins"`
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(&file) != nil || d.Decode(new(any)) != io.EOF || file.Plugins == nil || len(file.Plugins) > 16 {
		return nil, errConfig
	}
	ids, tokens := map[string]bool{}, map[string]bool{}
	for _, token := range reservedTokens {
		if token != "" {
			tokens[token] = true
		}
	}
	for i := range file.Plugins {
		c := &file.Plugins[i]
		if !pluginsdk.ValidID(c.ID) || !pluginsdk.ValidVersion(c.Version) || ids[c.ID] || c.Permissions == nil || len(c.Permissions) > len(knownPermissions) {
			return nil, errConfig
		}
		ids[c.ID] = true
		if c.APIVersion != "" && !pluginsdk.ValidVersion(c.APIVersion) {
			return nil, errConfig
		}
		seenPermissions := map[string]bool{}
		for _, p := range c.Permissions {
			if !slices.Contains(knownPermissions, p) || seenPermissions[p] {
				return nil, errConfig
			}
			seenPermissions[p] = true
		}
		c.Endpoint, err = pluginsdk.Endpoint(c.Endpoint, c.AllowInsecureHTTP)
		if err != nil {
			return nil, errConfig
		}
		if c.CAFile != "" {
			name := c.CAFile
			if !filepath.IsAbs(name) {
				name = filepath.Join(filepath.Dir(path), name)
			}
			if !strings.HasPrefix(c.Endpoint, "https://") {
				return nil, errConfig
			}
			if c.roots, err = loadRoots(name); err != nil {
				return nil, errConfig
			}
		}
		for _, secret := range []struct {
			path string
			out  *string
		}{{c.HostTokenFile, &c.hostToken}, {c.ServiceTokenFile, &c.serviceToken}} {
			name := secret.path
			if name == "" {
				return nil, errConfig
			}
			if !filepath.IsAbs(name) {
				name = filepath.Join(filepath.Dir(path), name)
			}
			data, err := readLimited(name, 1024)
			value := strings.TrimSpace(string(data))
			if err != nil || !pluginsdk.ValidToken(value) || tokens[value] {
				return nil, errConfig
			}
			tokens[value], *secret.out = true, value
		}
	}
	slices.SortFunc(file.Plugins, func(a, b Config) int { return strings.Compare(a.ID, b.ID) })
	return file.Plugins, nil
}
