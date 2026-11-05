package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var web embed.FS

type collectorStatus struct {
	Enabled       bool `json:"enabled"`
	CapturePoints []struct {
		Pipeline    string `json:"pipeline"`
		Point       string `json:"point"`
		Signal      string `json:"signal"`
		Sensitivity string `json:"sensitivity"`
	} `json:"capture_points"`
	Sessions []struct {
		SessionID string          `json:"session_id"`
		State     string          `json:"state"`
		Reason    string          `json:"reason"`
		ExpiresAt time.Time       `json:"expires_at"`
		Stats     json.RawMessage `json:"stats"`
	} `json:"sessions"`
}
type labServer struct {
	store      *captureStore
	generator  *generator
	controlURL string
	client     *http.Client
}

func respond(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, code int, err error) {
	respond(w, code, map[string]string{"error": err.Error()})
}
func input(w http.ResponseWriter, r *http.Request, target any) error {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		return fmt.Errorf("application/json is required")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func (s *labServer) control(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, s.controlURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("the Collector control is unavailable: %w", err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return encoded, response.StatusCode, err
}
func (s *labServer) collector(ctx context.Context) (*collectorStatus, error) {
	encoded, code, err := s.control(ctx, http.MethodGet, "/v1/status", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("the Collector status returned HTTP %d", code)
	}
	var status collectorStatus
	err = json.Unmarshal(encoded, &status)
	return &status, err
}

type captureOptions struct {
	Signals         []string `json:"signals"`
	Points          []string `json:"points"`
	Service         string   `json:"service"`
	ErrorsOnly      bool     `json:"errors_only"`
	DurationSeconds int      `json:"duration_seconds"`
	SamplingRate    *float64 `json:"sampling_rate"`
	OTTL            struct {
		Resource  []string `json:"resource"`
		Span      []string `json:"span"`
		Log       []string `json:"log"`
		Metric    []string `json:"metric"`
		Datapoint []string `json:"datapoint"`
	} `json:"ottl"`
}

func captureRequest(options captureOptions) ([]byte, error) {
	if options.DurationSeconds < 5 || options.DurationSeconds > 300 {
		return nil, fmt.Errorf("duration must be between 5 and 300 seconds")
	}
	if len(options.Signals) < 1 || len(options.Signals) > 3 || len(options.Points) < 1 || len(options.Points) > 2 {
		return nil, fmt.Errorf("choose at least one signal and capture point")
	}
	if options.Service != "" && !slices.Contains([]string{"checkout", "inventory", "billing"}, options.Service) {
		return nil, fmt.Errorf("unknown synthetic service")
	}
	rate := 1.0
	if options.SamplingRate != nil {
		rate = *options.SamplingRate
	}
	if rate < 0 || rate > 1 {
		return nil, fmt.Errorf("sampling rate must be between 0 and 1")
	}
	points := make([]map[string]string, 0)
	seen := make(map[string]bool)
	for _, signal := range options.Signals {
		if !slices.Contains([]string{"traces", "logs", "metrics"}, signal) || seen[signal] {
			return nil, fmt.Errorf("invalid or duplicate signal")
		}
		seen[signal] = true
		pointSet := make(map[string]bool)
		for _, point := range options.Points {
			if !slices.Contains([]string{"ingress", "egress"}, point) || pointSet[point] {
				return nil, fmt.Errorf("invalid or duplicate capture point")
			}
			pointSet[point] = true
			points = append(points, map[string]string{"pipeline": signal, "point": point})
		}
	}
	filter := map[string]any{}
	if options.Service != "" {
		filter["resource"] = []string{`attributes["service.name"] == ` + strconv.Quote(options.Service)}
	}
	if options.ErrorsOnly {
		filter["traces"] = map[string]any{"span": []string{`status.code == STATUS_CODE_ERROR`}}
		filter["logs"] = map[string]any{"log": []string{`severity_number >= SEVERITY_NUMBER_WARN`}}
		filter["metrics"] = map[string]any{"datapoint": []string{`attributes["http.response.status_code"] >= 500`}}
	}
	// Preserve the quick filters: every supplied condition is ANDed in its context.
	for _, level := range []struct {
		context, signal, key string
		expressions          []string
	}{
		{"resource", "", "resource", options.OTTL.Resource},
		{"span", "traces", "span", options.OTTL.Span},
		{"log", "logs", "log", options.OTTL.Log},
		{"metric", "metrics", "metric", options.OTTL.Metric},
		{"datapoint", "metrics", "datapoint", options.OTTL.Datapoint},
	} {
		if len(level.expressions) == 0 {
			continue
		}
		if level.signal != "" && !slices.Contains(options.Signals, level.signal) {
			return nil, fmt.Errorf("%s OTTL requires the %s signal", level.context, level.signal)
		}
		target := filter
		if level.signal != "" {
			if target[level.signal] == nil {
				target[level.signal] = map[string]any{}
			}
			target = target[level.signal].(map[string]any)
		}
		existing, _ := target[level.key].([]string)
		for _, expression := range level.expressions {
			expression = strings.TrimSpace(expression)
			if expression == "" {
				return nil, fmt.Errorf("%s OTTL contains an empty condition", level.context)
			}
			existing = append(existing, expression)
		}
		target[level.key] = existing
	}
	return json.Marshal(map[string]any{"session_id": fmt.Sprintf("lab-%x", time.Now().UnixNano()), "principal": "local-testlab", "target": map[string]string{"collector_group": "testlab"}, "signals": options.Signals, "capture_points": points, "filter": filter, "sampling": map[string]any{"rate": rate, "mode": "hash"}, "expires_at": time.Now().Add(time.Duration(options.DurationSeconds) * time.Second).UTC()})
}
func (s *labServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.collector(r.Context())
		message := ""
		if err != nil {
			message = err.Error()
		}
		respond(w, 200, map[string]any{"collector": status, "collector_error": message, "generator": s.generator.snapshot(), "buffer": s.store.stats()})
	})
	mux.HandleFunc("POST /api/capture", func(w http.ResponseWriter, r *http.Request) {
		var options captureOptions
		if err := input(w, r, &options); err != nil {
			failure(w, 400, err)
			return
		}
		request, err := captureRequest(options)
		if err != nil {
			failure(w, 400, err)
			return
		}
		encoded, code, err := s.control(r.Context(), http.MethodPost, "/v1/sessions", request)
		if err != nil {
			failure(w, 502, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(encoded)
	})
	mux.HandleFunc("DELETE /api/capture", func(w http.ResponseWriter, r *http.Request) {
		status, err := s.collector(r.Context())
		if err != nil {
			failure(w, 502, err)
			return
		}
		for _, session := range status.Sessions {
			if session.State == "active" || session.State == "stopping" {
				_, code, err := s.control(r.Context(), http.MethodDelete, "/v1/sessions/"+url.PathEscape(session.SessionID), nil)
				if err != nil {
					failure(w, 502, err)
					return
				}
				if code != 204 {
					failure(w, http.StatusBadGateway, fmt.Errorf("the Collector stop returned HTTP %d", code))
					return
				}
			}
		}
		respond(w, 200, map[string]bool{"stopped": true})
	})
	mux.HandleFunc("POST /api/generator", func(w http.ResponseWriter, r *http.Request) {
		var options struct {
			Enabled *bool `json:"enabled"`
			Rate    int   `json:"rate"`
		}
		if err := input(w, r, &options); err != nil {
			failure(w, 400, err)
			return
		}
		if options.Enabled == nil || options.Rate < 1 || options.Rate > 100 {
			failure(w, 400, fmt.Errorf("enabled and a rate between 1 and 100 are required"))
			return
		}
		s.generator.configure(*options.Enabled, options.Rate)
		respond(w, 200, s.generator.snapshot())
	})
	mux.HandleFunc("GET /api/captures", func(w http.ResponseWriter, r *http.Request) {
		after := uint64(0)
		var err error
		if raw := r.URL.Query().Get("after"); raw != "" {
			after, err = strconv.ParseUint(raw, 10, 64)
			if err != nil {
				failure(w, 400, fmt.Errorf("invalid cursor"))
				return
			}
		}
		respond(w, 200, s.store.page(after))
	})
	mux.HandleFunc("DELETE /api/captures", func(w http.ResponseWriter, _ *http.Request) {
		s.store.clear()
		respond(w, 200, map[string]bool{"cleared": true})
	})
	files, _ := fs.Sub(web, "web")
	static := http.FileServer(http.FS(files))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/app.css" {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			failure(w, 403, fmt.Errorf("test lab accepts localhost requests only"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Host != r.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				failure(w, 403, fmt.Errorf("cross-origin requests are not allowed"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
