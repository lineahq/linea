package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
	scenariofile "github.com/lineahq/linea/internal/debugger/scenario"
)

// Connected mode sends a command to a running linea-server instead of
// starting a local platform. The CLI reads each file argument and sends its
// text inline, labelled with the argument, so reports name the same paths as
// local runs. The server need not share the CLI's filesystem: the files a
// scenario names (inputs, mocks.files, mocks.kubernetes.files) are read here
// and written into it. Only a configuration's own ${file:...} references
// stay reads on the server, unless the scenario mocks them.

// serverFlag registers --server, and --project for the project the
// command works in there.
func serverFlag(fs *flag.FlagSet) *string {
	fs.StringVar(&workProject, "project", strings.TrimSpace(os.Getenv(projectEnv)),
		"with --server, the project the command works in: where new work and saved artifacts go, whose runtimes and saved work it uses, and what lists show (default $"+projectEnv+", else the default project)")
	return fs.String("server", "", "send the command to a team server at this URL, for example the local Compose stack at http://127.0.0.1:7070; $"+serverTokenEnv+" or the sign-in of linea login authenticates it")
}

// serverTokenEnv holds the API token the CLI presents to --server.
const serverTokenEnv = "LINEA_TOKEN"

// projectEnv names the project --project defaults to.
const projectEnv = "LINEA_PROJECT"

// workProject is the command's --project; a process runs one command.
var workProject string

// connect returns the client for --server, presenting $LINEA_TOKEN
// when set, else the stored sign-in of linea login; either way it
// trusts the CA that sign-in kept. --verbose writes local Collector logs,
// which a server run does not produce here.
func connect(server string, verbose bool) (*client.Client, error) {
	if verbose {
		return nil, errors.New("--verbose writes the logs of a local Collector; it cannot be used with --server")
	}
	token := strings.TrimSpace(os.Getenv(serverTokenEnv))
	cred, err := signedIn(server, token == "")
	if err != nil {
		return nil, err
	}
	var ca []byte
	if cred != nil {
		ca = []byte(cred.CA)
		if token == "" {
			token = cred.IDToken
		}
	}
	c, err := client.NewWithCA(server, token, ca)
	if err != nil {
		return nil, err
	}
	return c.InProject(workProject), nil
}

// readArg reads a file argument for an inline request.
func readArg(flagName, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--%s: %w", flagName, err)
	}
	return string(data), nil
}

// scenarioSource reads a scenario argument with the files it names inlined,
// so the server needs no access to the scenario's directory.
func scenarioSource(path string) (string, error) {
	text, err := readArg("scenario", path)
	if err != nil {
		return "", err
	}
	raw, err := scenariofile.Inline([]byte(text), filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("--scenario %s: %w", path, err)
	}
	return string(raw), nil
}

func inspectConnected(ctx context.Context, c *client.Client, config, scenario, runtimeName, format string, stdout, stderr io.Writer) int {
	var req api.InspectRequest
	var err error
	if req.ConfigYAML, err = readArg("config", config); err != nil {
		return fail(stderr, err)
	}
	req.ConfigLabel = config
	req.Runtime = runtimeName
	if scenario != "" {
		if req.ScenarioYAML, err = scenarioSource(scenario); err != nil {
			return fail(stderr, err)
		}
	}
	raw, ins, err := c.Inspect(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	if format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = writeInspection(stdout, ins)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if !ins.Valid {
		return exitError
	}
	return exitOK
}

func runConnected(ctx context.Context, c *client.Client, req api.Request, format string, verify bool, stdout, stderr io.Writer) int {
	var err error
	if req.ConfigYAML, err = readArg("config", req.ConfigLabel); err != nil {
		return fail(stderr, err)
	}
	if req.FixtureLabel != "" {
		if req.FixtureJSON, err = readArg("fixture", req.FixtureLabel); err != nil {
			return fail(stderr, err)
		}
	}
	if req.ScenarioLabel != "" {
		if req.ScenarioYAML, err = scenarioSource(req.ScenarioLabel); err != nil {
			return fail(stderr, err)
		}
	}
	id, raw, rep, err := c.RunWithID(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	var verified []verification
	if verify {
		if verified, err = verifyAll(rep, verifyConnected(ctx, c, id)); err != nil {
			return fail(stderr, err)
		}
	}
	if err := writeRun(stdout, stderr, format, rep, raw, verified); err != nil {
		return fail(stderr, err)
	}
	if rep.Mismatch() {
		return exitMismatch
	}
	return exitOK
}

// testConnected submits the scenarios as suites; the client splits more than
// platform.MaxSuiteMembers into consecutive suites.
func testConnected(ctx context.Context, c *client.Client, req platform.SuiteRequest) (*platform.SuiteReport, error) {
	text, err := readArg("config", req.Config)
	if err != nil {
		return nil, err
	}
	suite := api.SuiteRequest{ConfigYAML: text, ConfigLabel: req.Config, Runtime: req.Runtime}
	for _, path := range req.Scenarios {
		text, err := scenarioSource(path)
		if err != nil {
			return nil, err
		}
		suite.Scenarios = append(suite.Scenarios, api.SuiteScenario{ScenarioYAML: text, ScenarioLabel: path})
	}
	return c.RunSuite(ctx, suite, req.Config)
}
