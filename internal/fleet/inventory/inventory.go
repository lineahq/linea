// Package inventory turns OpAMP agent reports into bounded, allowlisted
// inventory fields. Raw configuration, environment and unknown attributes are
// dropped here and never reach storage or logs.
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.yaml.in/yaml/v3"
)

// Bounds per agent record: each record stays within 64 KiB.
const (
	MaxAttributes      = 64
	MaxOperatorKeys    = 16
	MaxComponents      = 256
	MaxPipelines       = 16
	MaxComponentsBytes = 48 * 1024
	MaxStatusBytes     = 64
	MaxErrorBytes      = 256
	// Larger effective configurations are hashed but not summarized.
	MaxConfigBytes = 256 * 1024
)

// builtin is the semantic-convention allowlist; service.* is a prefix.
var builtin = map[string]bool{
	"host.name": true, "host.arch": true, "os.type": true, "os.description": true,
	"k8s.cluster.name": true, "k8s.namespace.name": true, "k8s.pod.name": true, "k8s.pod.uid": true,
	"k8s.deployment.name": true, "k8s.statefulset.name": true, "k8s.daemonset.name": true, "k8s.node.name": true,
	"cloud.provider": true, "cloud.region": true, "cloud.availability_zone": true,
}

var kinds = map[string]bool{"receiver": true, "processor": true, "exporter": true, "connector": true, "extension": true}

var ErrInvalidKeys = errors.New("invalid inventory attribute keys")

// Ingest applies the installation's allowlist. It is safe for concurrent use.
type Ingest struct {
	extra        map[string]bool
	healthErrors bool
	dropped      atomic.Uint64
	unsummarized atomic.Uint64
}

// New accepts up to 16 operator attribute keys. healthErrors opts into storing
// redacted, truncated health error text.
func New(keys []string, healthErrors bool) (*Ingest, error) {
	if len(keys) > MaxOperatorKeys {
		return nil, ErrInvalidKeys
	}
	extra := map[string]bool{}
	for _, k := range keys {
		if !fleet.ValidKey(k) {
			return nil, ErrInvalidKeys
		}
		extra[k] = true
	}
	return &Ingest{extra: extra, healthErrors: healthErrors}, nil
}

// Allowed reports whether a key is stored.
func (i *Ingest) Allowed(key string) bool {
	return builtin[key] || strings.HasPrefix(key, "service.") || i.extra[key]
}

// Dropped counts attributes discarded by the allowlist or bounds.
func (i *Ingest) Dropped() uint64 { return i.dropped.Load() }

// Unsummarized counts effective configurations that were only hashed.
func (i *Ingest) Unsummarized() uint64 { return i.unsummarized.Load() }

// Attributes keeps allowlisted string-like values. Identifying attributes win
// over non-identifying ones with the same key.
func (i *Ingest) Attributes(d *protobufs.AgentDescription) map[string]string {
	out := map[string]string{}
	add := func(list []*protobufs.KeyValue, override bool) {
		for _, kv := range list {
			if kv == nil {
				continue
			}
			v, ok := scalar(kv.Value)
			if !ok || !i.Allowed(kv.Key) || !fleet.ValidKey(kv.Key) || !fleet.ValidValue(v) {
				i.dropped.Add(1)
				continue
			}
			if _, exists := out[kv.Key]; exists && !override {
				continue
			}
			out[kv.Key] = v
		}
	}
	add(d.GetNonIdentifyingAttributes(), false)
	add(d.GetIdentifyingAttributes(), true)
	if len(out) > MaxAttributes {
		keys := slices.Sorted(func(yield func(string) bool) {
			for k := range out {
				if !yield(k) {
					return
				}
			}
		})
		for _, k := range keys[MaxAttributes:] {
			delete(out, k)
			i.dropped.Add(1)
		}
	}
	return out
}

func scalar(v *protobufs.AnyValue) (string, bool) {
	switch x := v.GetValue().(type) {
	case *protobufs.AnyValue_StringValue:
		// Blank values carry no placement information and are dropped.
		return strings.TrimSpace(x.StringValue), true
	case *protobufs.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue), true
	case *protobufs.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10), true
	case *protobufs.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64), true
	}
	return "", false
}

// Health converts a health report. Component entries are keyed kind:name and
// carry the pipelines whose health map contains them.
func (i *Ingest) Health(h *protobufs.ComponentHealth) (fleet.Status, map[string]*fleet.Component) {
	status := fleet.Status{Status: printable(h.GetStatus(), MaxStatusBytes)}
	if i.healthErrors {
		status.Error = Redact(h.GetLastError())
	}
	components := map[string]*fleet.Component{}
	var walk func(map[string]*protobufs.ComponentHealth, string, int)
	walk = func(m map[string]*protobufs.ComponentHealth, pipeline string, depth int) {
		for _, key := range slices.Sorted(mapKeys(m)) {
			child := m[key]
			if child == nil {
				continue
			}
			if name, ok := strings.CutPrefix(key, "pipeline:"); ok && depth < 2 {
				walk(child.ComponentHealthMap, name, depth+1)
				continue
			}
			if len(child.ComponentHealthMap) > 0 && depth < 2 {
				walk(child.ComponentHealthMap, pipeline, depth+1)
				continue
			}
			if !validComponent(key) || len(components) >= MaxComponents && components[key] == nil {
				continue
			}
			c := components[key]
			if c == nil {
				c = &fleet.Component{ID: key}
				components[key] = c
			}
			healthy := child.Healthy
			// A component unhealthy in any pipeline is unhealthy.
			if c.Healthy == nil || !healthy {
				c.Healthy = &healthy
				c.Status = printable(child.Status, MaxStatusBytes)
				if i.healthErrors {
					c.Error = Redact(child.LastError)
				}
			}
			addPipeline(c, pipeline)
		}
	}
	walk(h.GetComponentHealthMap(), "", 0)
	return status, components
}

// MaxRemoteConfigHash bounds the echoed remote config hash, in bytes.
const MaxRemoteConfigHash = 64

// RemoteConfig converts a RemoteConfigStatus. Error text follows the health
// error opt-in, since Collector validation errors can quote configuration.
func (i *Ingest) RemoteConfig(st *protobufs.RemoteConfigStatus) fleet.RemoteConfig {
	var rc fleet.RemoteConfig
	if h := st.GetLastRemoteConfigHash(); len(h) > 0 && len(h) <= MaxRemoteConfigHash {
		rc.Hash = hex.EncodeToString(h)
	}
	switch st.GetStatus() {
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING:
		rc.Status = "applying"
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
		rc.Status = "applied"
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
		rc.Status = "failed"
	}
	if i.healthErrors && rc.Status == "failed" {
		rc.Error = Redact(st.GetErrorMessage())
	}
	return rc
}

// Config hashes the effective configuration and, when it parses within the
// size bound, lists component IDs with their pipelines. The configuration
// itself is discarded.
func (i *Ingest) Config(c *protobufs.EffectiveConfig) (string, map[string]*fleet.Component) {
	files := c.GetConfigMap().GetConfigMap()
	if len(files) == 0 {
		return "", nil
	}
	hash := sha256.New()
	names := slices.Sorted(mapKeys(files))
	for _, name := range names {
		f := files[name]
		for _, part := range []string{name, f.GetContentType()} {
			hash.Write([]byte(strconv.Itoa(len(part)) + ":" + part))
		}
		hash.Write([]byte(strconv.Itoa(len(f.GetBody())) + ":"))
		hash.Write(f.GetBody())
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	components := map[string]*fleet.Component{}
	for _, name := range names {
		body := files[name].GetBody()
		if len(body) > MaxConfigBytes || !summarize(body, components) {
			i.unsummarized.Add(1)
			return sum, nil
		}
	}
	return sum, components
}

type collectorConfig struct {
	Receivers  map[string]yaml.Node `yaml:"receivers"`
	Processors map[string]yaml.Node `yaml:"processors"`
	Exporters  map[string]yaml.Node `yaml:"exporters"`
	Connectors map[string]yaml.Node `yaml:"connectors"`
	Extensions map[string]yaml.Node `yaml:"extensions"`
	Service    struct {
		Extensions []string `yaml:"extensions"`
		Pipelines  map[string]struct {
			Receivers  []string `yaml:"receivers"`
			Processors []string `yaml:"processors"`
			Exporters  []string `yaml:"exporters"`
		} `yaml:"pipelines"`
	} `yaml:"service"`
}

func summarize(body []byte, components map[string]*fleet.Component) bool {
	var cfg collectorConfig
	if yaml.Unmarshal(body, &cfg) != nil {
		return false
	}
	add := func(id, pipeline string) {
		if !validComponent(id) {
			return
		}
		c := components[id]
		if c == nil {
			if len(components) >= MaxComponents {
				return
			}
			c = &fleet.Component{ID: id}
			components[id] = c
		}
		addPipeline(c, pipeline)
	}
	for kind, section := range map[string]map[string]yaml.Node{"receiver": cfg.Receivers, "processor": cfg.Processors, "exporter": cfg.Exporters, "connector": cfg.Connectors, "extension": cfg.Extensions} {
		for name := range section {
			add(kind+":"+name, "")
		}
	}
	for _, name := range cfg.Service.Extensions {
		add("extension:"+name, "")
	}
	for pipeline, p := range cfg.Service.Pipelines {
		// Connectors appear in pipelines as receivers or exporters.
		place := func(kind string, names []string) {
			for _, name := range names {
				if _, ok := cfg.Connectors[name]; ok && kind != "processor" {
					add("connector:"+name, pipeline)
				} else {
					add(kind+":"+name, pipeline)
				}
			}
		}
		place("receiver", p.Receivers)
		place("processor", p.Processors)
		place("exporter", p.Exporters)
	}
	return true
}

// Merge combines configured and reported components into a sorted, bounded
// list. The result is never nil, so it marks the section as reported.
func Merge(configured, reported map[string]*fleet.Component) []fleet.Component {
	merged := map[string]*fleet.Component{}
	for _, source := range []map[string]*fleet.Component{configured, reported} {
		for id, c := range source {
			m := merged[id]
			if m == nil {
				if len(merged) >= MaxComponents {
					continue
				}
				m = &fleet.Component{ID: id}
				merged[id] = m
			}
			for _, p := range c.Pipelines {
				addPipeline(m, p)
			}
			if c.Healthy != nil {
				h := *c.Healthy
				m.Healthy, m.Status, m.Error = &h, c.Status, c.Error
			}
		}
	}
	out := make([]fleet.Component, 0, len(merged))
	for _, id := range slices.Sorted(mapKeys(merged)) {
		c := *merged[id]
		sort.Strings(c.Pipelines)
		out = append(out, c)
	}
	return bound(out)
}

// bound keeps the serialized list within its byte budget: error text goes
// first, then trailing components.
func bound(list []fleet.Component) []fleet.Component {
	if size(list) <= MaxComponentsBytes {
		return list
	}
	for i := range list {
		list[i].Error = ""
	}
	for len(list) > 0 && size(list) > MaxComponentsBytes {
		list = list[:len(list)*3/4]
	}
	return list
}

func size(list []fleet.Component) int {
	b, _ := json.Marshal(list)
	return len(b)
}

func addPipeline(c *fleet.Component, pipeline string) {
	if pipeline == "" || !validName(pipeline) || slices.Contains(c.Pipelines, pipeline) || len(c.Pipelines) >= MaxPipelines {
		return
	}
	c.Pipelines = append(c.Pipelines, pipeline)
}

func validComponent(id string) bool {
	kind, name, ok := strings.Cut(id, ":")
	return ok && kinds[kind] && validName(name) && len(id) <= fleet.MaxKeyBytes
}

func validName(name string) bool {
	return name != "" && len(name) <= fleet.MaxKeyBytes && printable(name, fleet.MaxKeyBytes) == name && !strings.ContainsRune(name, ' ')
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// printable drops control characters and truncates on a rune boundary.
func printable(s string, limit int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

var (
	urlPattern    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)
	secretPattern = regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api[_-]?key|authorization)\b(\s*[:=]\s*)((?:bearer\s+|basic\s+)?(?:"[^"]*"|'[^']*'|\S+))`)
	schemePattern = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)\S+`)
	tokenPattern  = regexp.MustCompile(`[A-Za-z0-9+/_=-]{24,}`)
)

// Redact removes likely secrets from health error text: URLs keep only their
// scheme and host, credential assignments and long tokens are masked.
func Redact(text string) string {
	text = printable(text, 4*MaxErrorBytes)
	text = urlPattern.ReplaceAllStringFunc(text, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "***"
		}
		return u.Scheme + "://" + u.Host
	})
	text = secretPattern.ReplaceAllString(text, "$1$2***")
	text = schemePattern.ReplaceAllString(text, "$1$2***")
	text = tokenPattern.ReplaceAllString(text, "***")
	return printable(text, MaxErrorBytes)
}
