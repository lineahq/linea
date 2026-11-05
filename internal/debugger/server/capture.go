package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// maxTokenFile bounds the fleet token file read at startup.
const maxTokenFile = 4 << 10

// fleetServeOptions are linea-server's fleet flags. Startup paths belong to the
// composing local owner, never to HTTP or MCP arguments. The token is read
// from a file so it never appears in arguments, logs or errors.
type fleetServeOptions struct {
	url, tokenFile string
	allowHTTP      bool
}

// load turns the flags into the platform's capture options. Without
// --fleet-url live capture is off.
func (o fleetServeOptions) load() (platform.CaptureOptions, error) {
	if o.url == "" {
		if o.tokenFile != "" || o.allowHTTP {
			return platform.CaptureOptions{}, errors.New("--fleet-token-file and --fleet-allow-http need --fleet-url")
		}
		return platform.CaptureOptions{}, nil
	}
	if o.tokenFile == "" {
		return platform.CaptureOptions{}, errors.New("--fleet-url needs --fleet-token-file")
	}
	f, err := os.Open(o.tokenFile)
	if err != nil {
		return platform.CaptureOptions{}, fmt.Errorf("--fleet-token-file: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return platform.CaptureOptions{}, fmt.Errorf("--fleet-token-file: %w", err)
	}
	if len(raw) > maxTokenFile {
		return platform.CaptureOptions{}, errors.New("--fleet-token-file exceeds 4 KiB")
	}
	fleet := platform.FleetOptions{Endpoint: o.url, Token: strings.TrimSpace(string(raw)), AllowHTTP: o.allowHTTP}
	if err := fleet.Validate(); err != nil {
		return platform.CaptureOptions{}, fmt.Errorf("--fleet-url/--fleet-token-file: %w", err)
	}
	return platform.CaptureOptions{Fleet: fleet}, nil
}
