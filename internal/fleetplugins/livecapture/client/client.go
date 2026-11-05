// Package client is a generic public-API capture consumer with no debugger.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	capture "github.com/lineahq/linea/internal/fleetplugins/livecapture"
	"github.com/lineahq/linea/pluginsdk"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

type Client struct {
	Endpoint, Token string
	HTTP            *http.Client
	OnReceipt       func(Summary)
}
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("capture API returned HTTP %d", e.Status) }
func New(endpoint, token string, allowHTTP bool) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("invalid public capture endpoint")
	}
	if u.Scheme == "http" && !allowHTTP {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("HTTP requires explicit protected-network acknowledgement")
		}
	}
	httpClient := pluginsdk.NewHTTPClient()
	httpClient.Timeout = 2 * time.Second
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), Token: token, HTTP: httpClient}, nil
}
func (c *Client) Close() { c.HTTP.CloseIdleConnections() }
func (c *Client) Request(ctx context.Context, method, path string, body, out any) (int, error) {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, errors.New("invalid public capture request")
		}
	}
	if len(encoded) > 64<<10 {
		return 0, errors.New("capture request exceeds bound")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+"/"+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, errors.New("invalid capture request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(req)
	if err != nil {
		return 0, errors.New("capture API transport failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return response.StatusCode, errors.New("capture API response exceeds bound")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response.StatusCode, &HTTPError{response.StatusCode}
	}
	if out != nil && response.StatusCode != 204 && json.Unmarshal(data, out) != nil {
		return response.StatusCode, errors.New("invalid capture API response")
	}
	return response.StatusCode, nil
}

type Summary struct {
	Records  map[string]int64 `json:"records"`
	Accepted int64            `json:"accepted_records"`
	Rejected int64            `json:"rejected_records"`
	Batches  int64            `json:"batches"`
}

func (c *Client) Consume(ctx context.Context, channel string, ratio float64, verifyRedaction bool) (Summary, error) {
	result := Summary{Records: map[string]int64{"traces": 0, "logs": 0, "metrics": 0}}
	if !pluginsdk.ValidUID(channel) || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
		return result, errors.New("invalid consumer options")
	}
	seen := map[string]capture.Acknowledgement{}
	lastSuccess := time.Now()
	for {
		select {
		case <-ctx.Done():
			return result, nil
		default:
		}
		var envelope capture.Envelope
		code, err := c.Request(ctx, "GET", "v1/channels/"+channel+"/next", nil, &envelope)
		if err != nil {
			var failed *HTTPError
			if errors.As(err, &failed) && (failed.Status == 401 || failed.Status == 403 || failed.Status == 404) {
				return result, err
			}
			if time.Since(lastSuccess) > 3*time.Second {
				return result, err
			}
			select {
			case <-ctx.Done():
				return result, nil
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		lastSuccess = time.Now()
		if code == 204 {
			select {
			case <-ctx.Done():
				return result, nil
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if !pluginsdk.ValidUID(envelope.AuthorityID) || !pluginsdk.ValidUID(envelope.PluginInstance) || !pluginsdk.ValidUID(envelope.PluginEpoch) || !pluginsdk.ValidUID(envelope.ID) || envelope.Encoding != "otlp-protobuf" || envelope.Records <= 0 || len(envelope.Data) > 256<<10 {
			return result, errors.New("invalid delivery envelope")
		}
		ack, duplicate := seen[envelope.ID]
		if !duplicate {
			if len(seen) >= 1024 {
				return result, errors.New("consumer receipt bound reached")
			}
			count, err := inspect(envelope.Signal, envelope.Data, verifyRedaction)
			if err != nil || count != envelope.Records {
				return result, errors.New("capture payload validation failed")
			}
			ack = capture.Acknowledgement{ID: envelope.ID, Accepted: int64(math.Floor(float64(count) * ratio))}
			seen[envelope.ID] = ack
			result.Records[envelope.Signal] += count
			result.Accepted += ack.Accepted
			result.Rejected += count - ack.Accepted
			result.Batches++
		}
		if _, err := c.Request(ctx, "POST", "v1/channels/"+channel+"/acks", ack, nil); err != nil {
			return result, err
		}
		if !duplicate && c.OnReceipt != nil {
			c.OnReceipt(result)
		}
	}
}
func safeAttributes(attributes pcommon.Map) bool {
	for _, key := range []string{"http.request.header.authorization", "db.query.text"} {
		if _, ok := attributes.Get(key); ok {
			return false
		}
	}
	return true
}
func inspect(signal string, data []byte, redacted bool) (int64, error) {
	bad := errors.New("invalid or unredacted OTLP capture")
	switch signal {
	case "traces":
		r := ptraceotlp.NewExportRequest()
		if r.UnmarshalProto(data) != nil {
			return 0, bad
		}
		if redacted {
			for _, resource := range r.Traces().ResourceSpans().All() {
				if !safeAttributes(resource.Resource().Attributes()) {
					return 0, bad
				}
				for _, scope := range resource.ScopeSpans().All() {
					for _, record := range scope.Spans().All() {
						if !safeAttributes(record.Attributes()) {
							return 0, bad
						}
					}
				}
			}
		}
		return int64(r.Traces().SpanCount()), nil
	case "logs":
		r := plogotlp.NewExportRequest()
		if r.UnmarshalProto(data) != nil {
			return 0, bad
		}
		if redacted {
			for _, resource := range r.Logs().ResourceLogs().All() {
				if !safeAttributes(resource.Resource().Attributes()) {
					return 0, bad
				}
				for _, scope := range resource.ScopeLogs().All() {
					for _, record := range scope.LogRecords().All() {
						if !safeAttributes(record.Attributes()) {
							return 0, bad
						}
					}
				}
			}
		}
		return int64(r.Logs().LogRecordCount()), nil
	case "metrics":
		r := pmetricotlp.NewExportRequest()
		if r.UnmarshalProto(data) != nil {
			return 0, bad
		}
		if redacted {
			for _, resource := range r.Metrics().ResourceMetrics().All() {
				if !safeAttributes(resource.Resource().Attributes()) {
					return 0, bad
				}
			}
		}
		return int64(r.Metrics().DataPointCount()), nil
	}
	return 0, bad
}
