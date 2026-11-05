package bisect

import (
	"fmt"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/confmap"
	"go.yaml.in/yaml/v3"
)

// Part is one configuration document the OpAMP supervisor merges: a local
// config file or a remote config entry.
type Part struct {
	Name string
	Body []byte
}

// Compose rebuilds the configuration the OpAMP supervisor gives its Collector
// from local config files, in order, and remote config entries, in entry
// name order (verified against opampsupervisor v0.163.0,
// docs/internals/fleet-config-layers.md). Each document is merged with
// confmap's merge: maps merge key by key, and any other value, lists
// included, is replaced, except that service::extensions lists are
// concatenated without duplicates and service::telemetry::resource::attributes
// lists are merged by attribute name. ${...} references are kept as text, as
// the supervisor keeps them, so the Collector expands them at startup.
//
// The supervisor's own documents (its OpAMP extension, own telemetry and the
// no-op pipeline it runs without remote config) are not part of the result:
// they never touch the data path.
func Compose(local, remote []Part) ([]byte, error) {
	remote = slices.Clone(remote)
	slices.SortStableFunc(remote, func(a, b Part) int { return strings.Compare(a.Name, b.Name) })
	conf := confmap.New()
	for _, p := range append(slices.Clone(local), remote...) {
		incoming, err := parse(p.Body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		if err := merge(conf, incoming); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
	}
	out, err := yaml.Marshal(conf.ToStringMap())
	if err != nil {
		return nil, fmt.Errorf("encode the merged configuration: %w", err)
	}
	return out, nil
}

func parse(body []byte) (*confmap.Conf, error) {
	if len(body) == 0 {
		return confmap.New(), nil
	}
	retrieved, err := confmap.NewRetrievedFromYAML(body)
	if err != nil {
		return nil, err
	}
	return retrieved.AsConf()
}

const (
	extensionsKey = "service::extensions"
	attributesKey = "service::telemetry::resource::attributes"
)

func merge(base, incoming *confmap.Conf) error {
	baseExtensions, incomingExtensions := list(base, extensionsKey), list(incoming, extensionsKey)
	baseAttributes, incomingAttributes := list(base, attributesKey), list(incoming, attributesKey)
	if err := base.Merge(incoming); err != nil {
		return err
	}
	if len(baseExtensions) > 0 || len(incomingExtensions) > 0 {
		if err := set(base, extensionsKey, unique(append(baseExtensions, incomingExtensions...))); err != nil {
			return err
		}
	}
	if len(baseAttributes) > 0 && len(incomingAttributes) > 0 {
		return set(base, attributesKey, byName(append(baseAttributes, incomingAttributes...)))
	}
	return nil
}

func list(conf *confmap.Conf, key string) []any {
	v, _ := conf.Get(key).([]any)
	return slices.Clone(v)
}

// set replaces the list at a "::" key path.
func set(conf *confmap.Conf, key string, v []any) error {
	path := strings.Split(key, "::")
	var overlay any = v
	for i := len(path) - 1; i >= 0; i-- {
		overlay = map[string]any{path[i]: overlay}
	}
	return conf.Merge(confmap.NewFromStringMap(overlay.(map[string]any)))
}

func unique(all []any) []any {
	out := make([]any, 0, len(all))
	for _, v := range all {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// byName keeps each named attribute once, at its first position with its
// last value; unnamed entries stay as they are.
func byName(all []any) []any {
	out := make([]any, 0, len(all))
	at := map[string]int{}
	for _, v := range all {
		m, _ := v.(map[string]any)
		name, _ := m["name"].(string)
		if name == "" {
			out = append(out, v)
			continue
		}
		if i, seen := at[name]; seen {
			out[i] = v
			continue
		}
		at[name] = len(out)
		out = append(out, v)
	}
	return out
}
