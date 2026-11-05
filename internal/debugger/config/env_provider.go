package config

import (
	"context"
	"strings"

	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
)

// scenarioEnvFactory wraps the upstream env provider so a scenario can supply
// ${env:NAME} values without mutating the process environment or the config.
// Scenario values take precedence; other names resolve exactly as upstream.
func scenarioEnvFactory(values map[string]string) confmap.ProviderFactory {
	upstream := envprovider.NewFactory()
	if len(values) == 0 {
		return upstream
	}
	return confmap.NewProviderFactory(func(ps confmap.ProviderSettings) confmap.Provider {
		return &scenarioEnv{Provider: upstream.Create(ps), values: values}
	})
}

type scenarioEnv struct {
	confmap.Provider
	values map[string]string
}

func (p *scenarioEnv) Retrieve(ctx context.Context, uri string, watcher confmap.WatcherFunc) (*confmap.Retrieved, error) {
	name, _, _ := strings.Cut(strings.TrimPrefix(uri, p.Scheme()+":"), ":-")
	if val, ok := p.values[name]; ok {
		if val == "" {
			return confmap.NewRetrieved("")
		}
		return confmap.NewRetrievedFromYAML([]byte(val))
	}
	return p.Provider.Retrieve(ctx, uri, watcher)
}
