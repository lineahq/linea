package runtime

import (
	"context"
	"encoding/json"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/manifest"
	"github.com/lineahq/linea/internal/debugger/mock"
)

// extensionNote is the execution note of every upstream extension.
const extensionNote = "extensions do not run; their settings are validated"

// customExtensionNote is the execution note of a custom extension.
const customExtensionNote = "a custom extension: it runs for real"

// isCustom reports whether the registry records a module for the component
// type and that module is not an upstream Collector module: a component
// of a runtime built with custom modules. Stand-ins and factories
// without a recorded module are not custom.
func isCustom(modules map[component.Type]string, t component.Type) bool {
	m, ok := modules[t]
	if !ok {
		return false
	}
	path, _, _ := strings.Cut(m, " ")
	return !manifest.Upstream(path)
}

// runExtensions is the extension factories of a run: a custom extension's
// real factory, created with its client endpoints simulated,
// and inert ones for the rest. With rec, a custom
// extension's own logs and metrics are recorded, and the
// debugger's status watcher is registered too; statusWatcherURIs adds it
// to the configuration.
func runExtensions(base otelcol.Factories, rec *instrument.Recorder, endpoints *mock.Endpoints) (map[component.Type]extension.Factory, error) {
	custom := func(id component.ID) bool { return isCustom(base.ExtensionModules, id.Type()) }
	var factories []extension.Factory
	watch := false
	for _, f := range unique(base.Extensions) {
		if custom(component.NewID(f.Type())) {
			f = endpoints.Extension(f)
			if rec != nil {
				f, watch = instrument.Extension(f, rec), true
			}
			factories = append(factories, f)
			continue
		}
		factories = append(factories, inertExtension(f))
	}
	if watch {
		factories = append(factories, instrument.StatusWatcher(rec, custom))
	}
	return otelcol.MakeFactoryMap(factories...)
}

// statusWatcherURIs are the yaml: overrides that add the status watcher to
// a configuration whose service starts a custom extension, after the
// configured ones; none otherwise, so the configuration runs as written.
func statusWatcherURIs(cfg *otelcol.Config, modules map[component.Type]string) ([]string, error) {
	ids := []string{}
	custom := false
	for _, id := range cfg.Service.Extensions {
		ids = append(ids, id.String())
		custom = custom || isCustom(modules, id.Type())
	}
	if !custom {
		return nil, nil
	}
	list, err := json.Marshal(append(ids, instrument.StatusWatcherType.String()))
	if err != nil {
		return nil, err
	}
	return []string{
		"yaml:extensions::" + instrument.StatusWatcherType.String() + ": {}",
		"yaml:service::extensions: " + string(list),
	}, nil
}

// storageType is the storage extension the distribution includes. Its inert
// stand-in still hands out a client, one that keeps nothing, so a component
// configured with storage (filelog's offsets, an exporter's persistent queue)
// starts as it would with an empty storage directory.
var storageType = component.MustNewType("file_storage")

// inertExtension replaces an extension factory with one that keeps its
// type, default configuration and stability, so settings decode and validate
// exactly as for the real extension, but creates an extension that does
// nothing: no listener, probe, storage or authenticator starts.
func inertExtension(f extension.Factory) extension.Factory {
	create := func(context.Context, extension.Settings, component.Config) (extension.Extension, error) {
		return inert{}, nil
	}
	if f.Type() == storageType {
		create = func(context.Context, extension.Settings, component.Config) (extension.Extension, error) {
			return inertStorage{}, nil
		}
	}
	return extension.NewFactory(f.Type(), f.CreateDefaultConfig, create, f.Stability())
}

type inert struct {
	component.StartFunc
	component.ShutdownFunc
}

// inertStorage is the inert storage extension: every client keeps nothing.
type inertStorage struct{ inert }

func (inertStorage) GetClient(context.Context, component.Kind, component.ID, string) (storage.Client, error) {
	return storage.NewNopClient(), nil
}
