package platform

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// maxArtifactLabel is the longest label, in bytes, the artifact stores accept.
const maxArtifactLabel = 1024

// yamlLabel names a saved configuration or scenario as a YAML file: a label
// ending in .yaml or .yml (any case) is kept, any other gets .yaml appended,
// so config.bin is saved as config.bin.yaml. Surrounding space is trimmed and
// an appended name stays within maxArtifactLabel. Other kinds and an empty
// label are unchanged. Every client saves through this rule, so the web
// client's yamlName must match it.
func yamlLabel(kind artifact.Kind, label string) string {
	if kind != artifact.Configuration && kind != artifact.Scenario {
		return label
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return label
	}
	lower := strings.ToLower(label)
	if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
		return label
	}
	const ext = ".yaml"
	if len(label) > maxArtifactLabel-len(ext) {
		cut := maxArtifactLabel - len(ext)
		for cut > 0 && !utf8.RuneStart(label[cut]) {
			cut--
		}
		label = strings.TrimRightFunc(label[:cut], unicode.IsSpace)
	}
	return label + ext
}

// saveArtifact saves req under its YAML label. Every platform operation that
// saves an artifact goes through it.
func saveArtifact(ctx context.Context, repository artifact.Repository, req artifact.SaveRequest) (*artifact.Saved, error) {
	req.Label = yamlLabel(req.Kind, req.Label)
	return repository.Save(ctx, req)
}
