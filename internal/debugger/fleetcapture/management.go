package fleetcapture

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// managementBytes bounds a management request: a config version body is at
// most 128 KiB of YAML, which JSON escaping can grow.
const managementBytes = 512 << 10

// NoRevision sends a management request without If-Match.
const NoRevision int64 = -1

// Manage sends one request to fleet's core management API (agents, groups,
// events, configs, assignments and rollouts) and decodes a 2xx body into out.
// path is below /api/v1/ and escapes its own segments. A revision of zero or
// more is sent as If-Match, which fleet requires to create, replace or delete
// groups and to change or delete configs. Fleet's admin role check is the only
// gate on writes.
func (c *Client) Manage(ctx context.Context, method, path string, query url.Values, revision int64, body, out any) error {
	if !strings.HasPrefix(path, "/api/v1/") || strings.Contains(path, "..") || strings.HasPrefix(path, "/api/v1/plugins") || strings.HasPrefix(path, "/api/v1/credentials") {
		return errors.New("invalid fleet management path")
	}
	var h http.Header
	if revision >= 0 {
		h = http.Header{}
		h.Set("If-Match", strconv.Quote(strconv.FormatInt(revision, 10)))
	}
	_, err := c.send(ctx, method, c.coreURL(path, query), h, body, out, managementBytes)
	return err
}
