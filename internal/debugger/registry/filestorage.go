package registry

import (
	"context"
	"os"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/storage/filestorage"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/extension"
)

// fileStorage is the file_storage factory with validation that does not read
// the host: upstream Validate requires its directories to exist on the
// machine that validates, which is the deployment's machine, not the
// debugger's. Every other check runs on the decoded upstream settings, and
// the effective configuration shows them unchanged. Extensions do not run
// in the debugger, so nothing ever opens the directory.
func fileStorage() extension.Factory {
	f := filestorage.NewFactory()
	return extension.NewFactory(f.Type(),
		func() component.Config { return &fileStorageConfig{cfg: f.CreateDefaultConfig().(*filestorage.Config)} },
		func(ctx context.Context, set extension.Settings, cfg component.Config) (extension.Extension, error) {
			return f.Create(ctx, set, cfg.(*fileStorageConfig).cfg)
		},
		f.Stability())
}

// fileStorageConfig holds the upstream settings in an unexported field, so
// the Collector's validation walk calls only its Validate.
type fileStorageConfig struct {
	cfg *filestorage.Config
}

func (c *fileStorageConfig) Unmarshal(conf *confmap.Conf) error { return conf.Unmarshal(c.cfg) }

// Marshal has a value receiver: the Collector's marshal hook looks for
// Marshaler on struct values.
func (c fileStorageConfig) Marshal(conf *confmap.Conf) error { return conf.Marshal(c.cfg) }

// Validate runs the upstream checks with the directories replaced by one
// that exists, so only the host's directories go unchecked.
func (c *fileStorageConfig) Validate() error {
	cfg := *c.cfg
	cfg.Directory = os.TempDir()
	if c.cfg.Compaction != nil {
		compaction := *c.cfg.Compaction
		compaction.Directory = os.TempDir()
		cfg.Compaction = &compaction
	}
	return cfg.Validate()
}
