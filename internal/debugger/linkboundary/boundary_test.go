package linkboundary

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const module = "github.com/lineahq/linea/"

// collectorFree lists the packages that must link no Collector component
// implementation: the team server binary and the packages it is composed of.
var collectorFree = []string{
	"cmd/linea-server",
	"internal/debugger/analysis",
	"internal/debugger/api",
	"internal/debugger/artifact",
	"internal/debugger/assertion",
	"internal/debugger/authz",
	"internal/debugger/bisect",
	"internal/debugger/catalog",
	"internal/debugger/client",
	"internal/debugger/comparison",
	"internal/debugger/config",
	"internal/debugger/container",
	"internal/debugger/engine",
	"internal/debugger/evidence",
	"internal/debugger/instrument",
	"internal/debugger/manifest",
	"internal/debugger/mcp",
	"internal/debugger/mock",
	"internal/debugger/platform",
	"internal/debugger/release",
	"internal/debugger/runresult",
	"internal/debugger/scenario",
	"internal/debugger/server",
	"internal/debugger/standin",
	"internal/debugger/store/memory",
	"internal/debugger/store/pg",
	"internal/debugger/telemetry",
	"internal/debugger/topology",
	"internal/debugger/upgrade",
	"internal/debugger/webui",
	"internal/debugger/worker",
}

// implementation matches a component implementation package by its last path
// element, such as tailsamplingprocessor. The x-prefixed API packages
// (xreceiver, xprocessor and the like) are interfaces, not implementations.
var implementation = regexp.MustCompile(`/[a-z0-9]+(receiver|processor|exporter|connector|extension)$`)

var api = regexp.MustCompile(`/x(receiver|processor|exporter|connector|extension)$`)

// executor lists the debugger packages that run a Collector in process.
var executor = []string{
	"internal/debugger/registry",
	"internal/debugger/runtime",
	"internal/debugger/engine/local",
	"internal/debugger/ottlreplay",
	"internal/debugger/featureset",
}

// forbidden reports why a dependency may not be linked, or "".
func forbidden(dep string) string {
	switch {
	case strings.HasPrefix(dep, "k8s.io/"):
		return "Kubernetes client"
	case implementation.MatchString(dep) && !api.MatchString(dep):
		return "component implementation"
	}
	for _, p := range executor {
		if dep == module+p || strings.HasPrefix(dep, module+p+"/") {
			return "in-process executor"
		}
	}
	return ""
}

func TestForbidden(t *testing.T) {
	for dep, want := range map[string]bool{
		"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor": true,
		"go.opentelemetry.io/collector/processor/xprocessor":                                        false,
		"go.opentelemetry.io/collector/receiver/otlpreceiver":                                       true,
		"go.opentelemetry.io/collector/pdata/ptrace":                                                false,
		"k8s.io/client-go/kubernetes":                                                               true,
		module + "internal/debugger/registry":                                                       true,
		module + "internal/debugger/engine":                                                         false,
		module + "internal/debugger/standin":                                                        false,
	} {
		if got := forbidden(dep) != ""; got != want {
			t.Errorf("forbidden(%q) = %v, want %v", dep, got, want)
		}
	}
}

func TestCollectorFreePackagesLinkNoImplementation(t *testing.T) {
	if testing.Short() {
		t.Skip("lists dependencies with the go command")
	}
	for _, p := range collectorFree {
		out, err := exec.Command("go", "list", "-deps", module+p).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", p, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			if why := forbidden(dep); why != "" {
				t.Errorf("%s links %s (%s)", p, dep, why)
			}
		}
	}
}
