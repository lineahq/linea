// Command gen writes the component registry of an OCB manifest and, with
// -ottl and -features, the OTTL facade's variant bindings for its Collector
// release and the optional features of its components, and with -gates the
// owners of the worker's feature gates (after -out, from the module's
// source, so its modules must be resolved), and with -release the
// Collector release line and modules. The built-in runtime
// runs it through go generate on build/linea-runtime.yaml; runtime builds
// generate the same files from the user's manifest.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

func main() {
	in := flag.String("manifest", "", "OCB manifest to read")
	out := flag.String("out", "", "Go file to write")
	ottlOut := flag.String("ottl", "", "OTTL variant bindings to write (internal/debugger/compat/ottl/variant.go)")
	featuresOut := flag.String("features", "", "feature set to write (internal/debugger/featureset/featureset.go)")
	source := flag.String("source", "", "manifest name for the generated header (default: the -manifest path)")
	providersOut := flag.String("providers", "", "debugger providers to write (internal/debugger/registry/providers.go), with -provider")
	var providers []manifest.ProviderImport
	flag.Func("provider", "a package exporting DebuggerProvider (repeatable)", func(v string) error {
		providers = append(providers, manifest.ProviderImport{Package: v})
		return nil
	})
	releaseOut := flag.String("release", "", "release line and modules to write (internal/debugger/release/collector.go)")
	gatesOut := flag.String("gates", "", "feature gate owners to write (internal/debugger/registry/gateowners.go); alone, from the existing components.go")
	flag.Parse()
	err := error(nil)
	if *in != "" || *out != "" {
		err = run(*in, *out, *ottlOut, *featuresOut, *releaseOut, *source)
	}
	if err == nil && *providersOut != "" {
		name := *source
		if name == "" {
			name = filepath.ToSlash(*in)
		}
		err = writeProviders(*providersOut, providers, name)
	}
	if err == nil && *gatesOut != "" {
		name := *source
		if name == "" {
			name = "the registry"
		}
		err = writeGates(*gatesOut, name)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(in, out, ottlOut, featuresOut, releaseOut, source string) error {
	if in == "" || out == "" {
		return fmt.Errorf("-manifest and -out are required")
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return err
	}
	if source == "" {
		source = filepath.ToSlash(in)
	}
	src, err := manifest.GenerateRegistry(m, manifest.RegistryOptions{
		Package: "registry", Source: source, Overrides: manifest.RegistryOverrides,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, src, 0o644); err != nil {
		return err
	}
	if ottlOut != "" {
		variant, err := manifest.GenerateOTTLVariant(m.CollectorVersion(), source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(ottlOut, variant, 0o644); err != nil {
			return err
		}
	}
	if releaseOut != "" {
		rel, err := manifest.GenerateRelease(m, source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(releaseOut, rel, 0o644); err != nil {
			return err
		}
	}
	if featuresOut != "" {
		set, err := manifest.GenerateFeatureSet(m, features.Packages(), source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(featuresOut, set, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func writeProviders(out string, providers []manifest.ProviderImport, source string) error {
	src, err := manifest.GenerateProviders(providers, source)
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644)
}
