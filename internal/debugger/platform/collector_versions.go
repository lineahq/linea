package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/lineahq/linea/build"
	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/catalog"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/release"
)

// The support table and catalogs: which Collector
// releases a runtime builds for, the components of each verified release,
// and manifests written from them. None of these needs Docker or the
// network.

type (
	// CollectorCatalog is a verified release's component catalog.
	CollectorCatalog = versions.Catalog
	// CatalogComponent is one catalog entry.
	CatalogComponent = versions.Component
)

// The manifest presets.
const (
	PresetAll     = catalog.PresetAll
	PresetBuiltIn = catalog.PresetBuiltIn
)

// CollectorSupport is the support table: releases from Oldest on build;
// Verified ones have a dependency set and a catalog, newer ones build
// unverified.
type CollectorSupport struct {
	Oldest   string
	Verified []string
	// BuiltIn is the built-in runtime's release.
	BuiltIn string
}

// GetCollectorSupport returns the support table.
func (s *Service) GetCollectorSupport(ctx context.Context, caller Caller) (_ *CollectorSupport, err error) {
	defer s.audit(&caller, "GetCollectorSupport", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	return &CollectorSupport{Oldest: versions.Oldest, Verified: s.versionTable().Versions(), BuiltIn: release.CollectorVersion}, nil
}

// GetCollectorCatalog returns a verified release's component catalog; an
// empty version means the newest verified release.
func (s *Service) GetCollectorCatalog(ctx context.Context, caller Caller, version string) (_ *CollectorCatalog, err error) {
	defer s.audit(&caller, "GetCollectorCatalog", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	r, err := s.verifiedRelease(version)
	if err != nil {
		return nil, err
	}
	return r.Catalog, nil
}

func (s *Service) verifiedRelease(version string) (*versions.Release, error) {
	table := s.versionTable()
	if version == "" {
		version = table.Newest()
	}
	if r := table.Release(version); r != nil {
		return r, nil
	}
	return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf(
		"Collector %s has no catalog: this debugger verified %s", strings.TrimPrefix(version, "v"), strings.Join(table.Versions(), ", "))}
}

// ComposeManifestRequest writes a manifest from a verified release's
// catalog: a preset, or the components a configuration uses together with
// picked ones.
type ComposeManifestRequest struct {
	// Version is the release; empty means the newest verified one.
	Version string
	Preset  string
	// Configuration selects every component type it defines.
	Configuration *Source
	// Components are "<kind>/<type>", such as "processors/transform".
	Components []string
	// Sources are custom module sources, as a build takes them: the
	// manifest gets an entry for each component package found in them.
	Sources []SourceOverride
}

// ComposedManifest is a written manifest. Unknown names the requested
// components the catalog does not know (custom components the user adds
// with their own module); Warnings name selected components the debugger
// does not support; Custom names the "<kind>/<import path>" entries added
// for the sources' components; Providers names the debugger providers
// found in the sources as "<name> <import path>", the name
// "?" when the source does not state it.
type ComposedManifest struct {
	Manifest  string
	Unknown   []string
	Warnings  []string
	Custom    []string
	Providers []string
}

// ComposeManifest writes an ordinary OCB manifest the user can edit, save
// or build. It never saves or builds.
func (s *Service) ComposeManifest(ctx context.Context, caller Caller, req ComposeManifestRequest) (_ *ComposedManifest, err error) {
	defer s.audit(&caller, "ComposeManifest", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	r, err := s.verifiedRelease(req.Version)
	if err != nil {
		return nil, err
	}
	cr := catalog.Request{Preset: req.Preset, Components: req.Components}
	var providers []string
	if cr.Custom, providers, err = s.customComponents(ctx, req.Sources); err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	if req.Configuration != nil {
		src := *req.Configuration
		if _, err := s.resolveSource(ctx, caller, &src, artifact.Configuration, "configuration"); err != nil {
			return nil, err
		}
		if cr.Config, err = (config.Source{Path: src.Path, YAML: src.Text}).BytesWith(s.policy.Paths); err != nil {
			return nil, classify(err, CodeValidationFailed)
		}
	}
	out, err := catalog.Compose(r.Catalog, build.Manifest, cr)
	if err != nil {
		return nil, &Error{Code: CodeInvalidRequest, Message: err.Error(), cause: err}
	}
	custom := make([]string, len(cr.Custom))
	for i, c := range cr.Custom {
		custom[i] = string(c.Kind) + "/" + c.Import
	}
	return &ComposedManifest{Manifest: string(out.Manifest), Unknown: out.Unknown, Warnings: out.Warnings, Custom: custom, Providers: providers}, nil
}
