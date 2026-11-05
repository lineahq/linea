package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// importEnvFlag registers --import-env: the host environment variables a
// local command's ${env:NAME} may read. Every other reference takes the
// scenario's declared value, its URI default or a stand-in, never the host.
// With --server, the server's own --import-env decides.
func importEnvFlag(fs *flag.FlagSet) *stringsFlag {
	var imports stringsFlag
	fs.Var(&imports, "import-env", "a host environment variable that ${env:NAME} may read (repeatable); others need a declared value. Ignored with --server")
	return &imports
}

// localEnvironment is the environment policy of a direct command:
// declared values only, plus the named host imports.
func localEnvironment(imports []string) (platform.EnvironmentPolicy, error) {
	policy := platform.EnvironmentPolicy{Host: imports}
	if err := policy.Validate(); err != nil {
		return platform.EnvironmentPolicy{}, fmt.Errorf("--import-env %s: %w", strings.Join(imports, ","), err)
	}
	return policy, nil
}
