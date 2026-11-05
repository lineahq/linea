package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// fleetCommands lists each fleet subcommand with the flags it takes beyond
// --server; required flags are marked with a trailing "!".
var fleetCommands = []struct {
	name, flags, summary string
}{
	{"overview", "group-by", "fleet's version, config management and agent counts"},
	{"agents", "selector connected healthy sort limit cursor", "page agents with the Collectors each links to live capture"},
	{"agent", "id!", "one agent with its components and remote config state"},
	{"retry", "id!", "resend a desired config the agent failed to apply"},
	{"events", "limit cursor", "page fleet's change events after the cursor"},
	{"group-save", "request!", "create a group (no id) or replace one at its revision"},
	{"group-delete", "id! revision!", "delete a group at its revision"},
	{"configs", "limit cursor", "page configs"},
	{"config", "id!", "read a config"},
	{"config-create", "request!", "create an empty config"},
	{"config-update", "id! request!", "rename or redescribe a config at its revision"},
	{"config-delete", "id! revision!", "delete a config and its versions at its revision"},
	{"versions", "id! limit cursor", "page a config's versions"},
	{"version", "id! version!", "read a version with its body"},
	{"version-create", "id! body! message", "store a config's next version from a YAML file"},
	{"assignments", "limit cursor", "page config assignments"},
	{"assignment", "id!", "read a config assignment"},
	{"assignment-create", "request!", "assign a config version and start its rollout"},
	{"preview", "id! limit cursor", "page the agents an assignment targets and their layers"},
	{"rollouts", "limit cursor", "page rollouts"},
	{"rollout", "id!", "read a rollout with its waves"},
	{"rollout-start", "request!", "move an assignment to another version, or remove it"},
	{"rollout-action", "id! action!", "pause, resume, rollback or cancel a rollout"},
	{"targets", "id! outcome limit cursor", "page a rollout's targets"},
}

func fleetUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: linea fleet <command> --server URL [flags]\n\ncommands:")
	for _, c := range fleetCommands {
		var flags []string
		for _, f := range strings.Fields(c.flags) {
			if name, ok := strings.CutSuffix(f, "!"); ok {
				flags = append(flags, "--"+name)
			} else {
				flags = append(flags, "[--"+f+"]")
			}
		}
		fmt.Fprintf(w, "  %-18s %s\n  %-18s   %s\n", c.name, strings.Join(flags, " "), "", c.summary)
	}
}

// fleetCmd manages fleet through the team server's fleet connection. Every
// command prints fleet's answer as JSON.
func fleetCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fleetUsage(stderr)
		return exitError
	}
	command := args[0]
	if command == "-h" || command == "--help" || command == "help" {
		fleetUsage(stdout)
		return exitOK
	}
	i := slices.IndexFunc(fleetCommands, func(c struct{ name, flags, summary string }) bool { return c.name == command })
	if i < 0 {
		return fail(stderr, fmt.Errorf("unknown fleet command %q", command))
	}
	spec := fleetCommands[i]
	fs := newFlagSet("fleet "+command, "--server URL [flags]", stderr)
	server := serverFlag(fs)
	id := fs.String("id", "", "agent, group, config, assignment or rollout ID")
	file := fs.String("request", "", "JSON request file")
	body := fs.String("body", "", "Collector YAML file of the new version")
	message := fs.String("message", "", "the new version's message")
	version := fs.Int("version", 0, "config version")
	revision := fs.Int64("revision", 0, "the current revision")
	action := fs.String("action", "", "pause, resume, rollback or cancel")
	outcome := fs.String("outcome", "", "only targets of this outcome")
	groupBy := fs.String("group-by", "", "promoted attribute to count agents by")
	selector := fs.String("selector", "", "fleet selector, such as service.name=cart")
	connected := fs.String("connected", "", "true or false: only connected or disconnected agents")
	healthy := fs.String("healthy", "", "true or false: only healthy or unhealthy agents")
	sort := fs.String("sort", "", "promoted attribute to sort agents by")
	limit := fs.Int("limit", 0, "page entries; zero uses the server's default")
	cursor := fs.String("cursor", "", "opaque cursor from the preceding page")
	if code, ok := parse(fs, args[1:], nil); !ok {
		return code
	}
	if *server == "" {
		return fail(stderr, fmt.Errorf("--server is required"))
	}
	if err := fleetFlagsMatch(fs, spec.flags); err != nil {
		return fail(stderr, err)
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	page := platform.CollectorPageRequest{Limit: *limit, Cursor: *cursor}
	var v any
	switch command {
	case "overview":
		v, err = c.GetFleetOverview(ctx, *groupBy)
	case "agents":
		q := platform.FleetAgentQuery{Limit: *limit, Cursor: *cursor, Selector: *selector, Sort: *sort}
		if q.Connected, err = optionalBool("connected", *connected); err == nil {
			if q.Healthy, err = optionalBool("healthy", *healthy); err == nil {
				v, err = c.ListFleetAgents(ctx, q)
			}
		}
	case "agent":
		v, err = c.GetFleetAgent(ctx, *id)
	case "retry":
		v, err = c.RetryFleetAgentConfig(ctx, *id)
	case "events":
		v, err = c.ListFleetEvents(ctx, page)
	case "group-save":
		v, err = fleetRequest(*file, func(q platform.CollectorGroupSave) (any, error) { return c.SaveCollectorGroup(ctx, q) })
	case "group-delete":
		v, err = fleetDeleted(c.DeleteCollectorGroup(ctx, *id, *revision))
	case "configs":
		v, err = c.ListFleetConfigs(ctx, page)
	case "config":
		v, err = c.GetFleetConfig(ctx, *id)
	case "config-create":
		v, err = fleetRequest(*file, func(q platform.FleetConfigCreate) (any, error) { return c.CreateFleetConfig(ctx, q) })
	case "config-update":
		v, err = fleetRequest(*file, func(q platform.FleetConfigUpdate) (any, error) { return c.UpdateFleetConfig(ctx, *id, q) })
	case "config-delete":
		v, err = fleetDeleted(c.DeleteFleetConfig(ctx, *id, *revision))
	case "versions":
		v, err = c.ListFleetConfigVersions(ctx, *id, page)
	case "version":
		v, err = c.GetFleetConfigVersion(ctx, *id, *version)
	case "version-create":
		var yaml []byte
		if yaml, err = readBounded(*body, 128<<10, "config version"); err == nil {
			v, err = c.CreateFleetConfigVersion(ctx, *id, platform.FleetConfigVersionCreate{Body: string(yaml), Message: *message})
		}
	case "assignments":
		v, err = c.ListFleetAssignments(ctx, page)
	case "assignment":
		v, err = c.GetFleetAssignment(ctx, *id)
	case "assignment-create":
		v, err = fleetRequest(*file, func(q platform.FleetAssignmentCreate) (any, error) { return c.CreateFleetAssignment(ctx, q) })
	case "preview":
		v, err = c.PreviewFleetAssignment(ctx, *id, page)
	case "rollouts":
		v, err = c.ListFleetRollouts(ctx, page)
	case "rollout":
		v, err = c.GetFleetRollout(ctx, *id)
	case "rollout-start":
		v, err = fleetRequest(*file, func(q platform.FleetRolloutStart) (any, error) { return c.StartFleetRollout(ctx, q) })
	case "rollout-action":
		v, err = c.ControlFleetRollout(ctx, *id, platform.FleetRolloutAction{Action: *action})
	case "targets":
		v, err = c.ListFleetRolloutTargets(ctx, *id, platform.FleetRolloutTargetQuery{Limit: *limit, Cursor: *cursor, Outcome: *outcome})
	}
	if err != nil {
		return fail(stderr, err)
	}
	if err = platform.Encode(stdout, v); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// fleetFlagsMatch refuses a flag the command does not take and reports a
// missing required one.
func fleetFlagsMatch(fs *flag.FlagSet, spec string) error {
	allowed := map[string]bool{"server": true}
	var required []string
	for _, f := range strings.Fields(spec) {
		name, must := strings.CutSuffix(f, "!")
		allowed[name] = true
		if must {
			required = append(required, name)
		}
	}
	set := map[string]bool{}
	var err error
	fs.Visit(func(f *flag.Flag) {
		set[f.Name] = true
		if !allowed[f.Name] && err == nil {
			err = fmt.Errorf("fleet %s does not take --%s", fs.Name()[len("linea fleet "):], f.Name)
		}
	})
	if err != nil {
		return err
	}
	for _, name := range required {
		if !set[name] {
			return fmt.Errorf("--%s is required", name)
		}
	}
	return nil
}

func optionalBool(name, raw string) (*bool, error) {
	if raw == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, fmt.Errorf("--%s takes true or false", name)
	}
	return &b, nil
}

func fleetDeleted(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]bool{"deleted": true}, nil
}

// readBounded reads a file of at most limit bytes.
func readBounded(path string, limit int64, what string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s exceeds %d KiB", what, limit>>10)
	}
	return raw, nil
}

// fleetRequest decodes one strict JSON document from file and sends it.
func fleetRequest[T any](file string, send func(T) (any, error)) (any, error) {
	raw, err := readBounded(file, 64<<10, "fleet request")
	if err != nil {
		return nil, err
	}
	var q T
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&q); err != nil {
		return nil, err
	}
	if err = d.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("expected one request document")
	}
	return send(q)
}
