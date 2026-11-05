# Path policy: request-triggered host reads

A request can name host files: a configuration path, a fixture or scenario
path, files a scenario refers to, and `${file:...}` references inside a
configuration (including nested ones such as `${file:${env:X}}`). This note
lists every place where such a request makes the service open a host file, so
that a path policy (allowed roots) can be enforced at
each of them and nowhere is missed.

The inventory comes from
`grep -rn "os\.Open\|os\.ReadFile\|os\.Stat\|os\.ReadDir" internal --include=*.go`
(and the same over `cmd/`), plus a review of what Collector components read
while a run executes. Sites are named by function, not by line.

## Read sites

| # | Call site | What it reads | Reached by | Enforcement |
| --- | --- | --- | --- | --- |
| 1 | `internal/debugger/engine/prepare.go` (`inputCapture.read`, opening through `hostfs.Policy.Open`) | the fixture file, the scenario file, and through `loadInputs` the scenario's `inputs[].file`, `mocks.files[].file` and `mocks.kubernetes.files[]` (relative names joined to the scenario directory; scenario text has none, so its files must be inline) | runs in both modes (`SubmitRun`, `ExecuteRun`, suite members through the same admission) | open through the policy; request paths are checked before `filepath.Abs` so a relative path is refused under roots |
| 2 | `internal/debugger/config/snapshot.go` (`captureProvider.Retrieve`, `file` branch, opening through `hostfs.Policy.Open`) | the configuration root given by path (`config.FileURI`) and every `${file:...}` URI the Collector's resolver retrieves, nested references included | runs (prepare), inspection and preview (they resolve through the same capture) | the capture opens through the policy; a refused configuration root is returned instead of the Collector's resolution error; a `${file:}` reference the policy refuses is never read and resolves to a stand-in with reason `outside_roots` |
| 3 | `internal/debugger/config/config_resolver.go` (`Source.Bytes`: `os.ReadFile`) | the configuration root, to hash it | inspection (`sourceOf` in `internal/debugger/engine/inspect.go`); the run provenance fallback in `provenance.go` is unreachable because `Execute` prepares first | `Source.BytesWith(policy)` |
| 4 | `internal/debugger/scenario/scenario.go` (`Load`: `os.ReadFile`) | the scenario file named by path | inspection and preview (`loadScenarioWith`, `internal/debugger/engine/policy.go`) | `scenario.LoadWith(path, reader)` with a reader from the policy |
| 5 | `internal/debugger/scenario/scenario.go` (`readFile`: `os.ReadFile` when no reader is set) | `mocks.files[].file` and `mocks.kubernetes.files[]` (inspection reads the mocks through `MockSpec`; preview reads only the environment) | inspection; runs already pass a reader (row 1) | the scenario is given the policy's reader (`WithFileReader`) |
| 6 | `internal/debugger/mock/builtin/filelog.go` (`hostPath`) feeding the real filelog receiver's `include` and `exclude` | the files the receiver globs while a run executes; patterns are mapped into the per-run sandbox, but `filepath.Join` cleans `..`, so a pattern such as `/var/log/../../../../x` leaves the sandbox | runs whose configuration has a filelog receiver | confine the mapping to the sandbox for every composition (a cleaned pattern can never leave it); the sandbox is never a host root |

## The opener

`internal/debugger/hostfs.Policy` is the only opener for these sites.
`Unrestricted` opens any path. Otherwise a relative path is refused ("use an
absolute path under an allowed root"); an absolute path is matched to a root
with `filepath.Rel` (case-insensitive on Windows) and opened with
`os.OpenRoot(root).Open(rel)`, which refuses `..` and links leaving the root.
The regular-file check uses the opened handle, so nothing is checked by name and
then opened. Refusals wrap `hostfs.ErrOutsideRoots` (or `ErrNoRoots` for a
policy without roots) and name the path and the roots, never its content, and
the platform adds the remedy (send the content inline, and the composition's
grant when it names one; a team server names none). A refused `${file:}`
reference is not an error: it stands in (below).

Verified on Go
1.27.2 (Windows): a symbolic link or directory junction leaving the root is
refused; a link with a relative target inside the root is followed; a link
with an absolute target is refused even when it points inside the root
(`os.Root` does not follow absolute targets). `os.Root`'s escape error is not
exported, so `hostfs` recognizes it by comparing with the error the same root
returns for a lexical `..`.

Scenario-relative names keep their meaning under roots: they are joined to the
scenario's directory first and the joined path is checked.

## Not request-triggered

These reads are listed so every hit of the search above is accounted for.

| Call site | Why it is out of the table |
| --- | --- |
| `internal/debugger/engine/bundle.go` | reads the debugger's own executable (`os.Executable`) for build identity |
| `internal/debugger/scenario/inline.go` (`Inline`: `os.ReadFile`) | the files a scenario names, read by the connected CLI (`cmd/linea/connected.go`) on its own machine so it can send them inline; the server never runs it, and the CLI is unrestricted |
| `cmd/linea/test.go` (`os.ReadDir`) | `test --tests DIR` on the command line; the CLI is unrestricted |
| `internal/debugger/mock/builtin/filelog.go` | a write (`os.OpenFile` for append) inside the per-run sandbox; mock file paths containing `..` are refused by `validateFiles` |

Collector components that run while a run executes and could read files:

- Receivers and exporters are substituted in the instrumented run; the
  reference run's preflight refuses receiver TLS and exporter TLS files, host
  trust stores and devices (`internal/debugger/runtime/reference_prerequisites.go`).
- `k8sattributes` talks to the in-process Kubernetes API mock with
  `auth_type: none`; it reads no service account files.
- `memory_limiter` reads fixed cgroup and memory paths of the host; a request
  cannot choose them.
- Frozen execution resolves `${file:...}` and `${env:...}` from the captured
  snapshot only; it reads no host file.
- Execution bundles are not re-checked against the roots: replay reads
  no host file.

Isolating components from the host filesystem in general (OS sandboxing) is
out of scope for the path policy.

## Enforcement

`engine.Policy{Environment, Paths}` is the composition's policy.
`PrepareWithPolicy`, `InspectWithPolicy` and `EvaluateOTTLWithPolicy` apply it;
the `*WithEnvironment` functions and `Prepare`, `Inspect` and `EvaluateOTTL`
keep unrestricted paths. The config capture takes it through
`config.CaptureOptions` and remembers a refusal of the configuration root, which
it returns instead of the Collector's resolution error: the refusal does not
depend on how the Collector wraps provider errors. A `${file:}` reference reads
the scenario's `mocks.files` entry with the same path first; otherwise a
refusal, a missing or an unreadable file resolves to the stand-in string
`linea-stand-in-<hash>` with reason `outside_roots`, `missing` or `unreadable`,
recorded in the snapshot and reported in `references`, as an inspection warning
`reference_stand_in`, and in the hint of any setting that cannot use the string.
The refused file is never opened. Inspection and preview return a refusal
(`config.Refused`) as an error, never as a diagnostic, including when it comes
from scenario text that would otherwise be reported as "not applied". Request
paths (config, fixture, scenario, a scenario directory) of an in-process
command are checked with `Policy.Abs` before anything makes them absolute; the
HTTP API and MCP have no path fields. Nothing scans YAML text for `${file:`: the
capture sees every URI the Collector's resolver retrieves, nested ones included.

Tests: `TestPathPolicyCoversEveryReadSite` (rows 1 to 5, for runs, inspection
and preview, each refused outside the roots, or standing in for a `${file:}`
reference, accepted inside and unrestricted),
`TestRelativePathsAreRefusedUnderRoots` (including a scenario directory; a
relative `${file:./x}` stands in), `TestUnavailableReferencesResolveToStandIns`,
`TestFilelogPatternsCannotLeaveTheSandbox` and `TestHostPathStaysInTheSandbox`
(row 6), and the `internal/debugger/hostfs` tests.

## Compositions

`platform.Options.Paths` carries the policy to every operation of a service.

| Composition | Policy |
| --- | --- |
| CLI direct commands (`inspect`, `run`, `test`, `diff`, `ottl`, `runtime build`, ...) | unrestricted |
| `linea-server --config` | no roots: inline content and saved revisions only. The server config has no setting for roots. There is no other `linea-server` mode |
| `api.New` without an injected platform | unrestricted |
| `platform.New` / `platform.Open` with zero `Paths` | no roots: inline content only |

A server reads no host file for a request. Its HTTP and MCP requests have no
path fields: a request that names one is `invalid_request` (HTTP 400). A file
a scenario still names is refused as `forbidden`, and a `${file:}` reference
stands in with reason `outside_roots`. Runtime sources are git repositories
the server clones; without an unrestricted policy `git clone` runs with
`protocol.file.allow=never`, so a local repository (a path or `file://`) is
refused. The connected CLI and the web client send files inline. Startup
prints that requests read no host file or host environment variable.

Roots are absolute, existing directories, fixed when the service is composed;
request content never adds one.
A refusal is the platform error `forbidden` (HTTP 403 on `POST /api/v1/runs`,
`/config/validate`, `/config/graph` and `/ottl/evaluate`). Its message is the
path error: the path the request named and why, never file contents. A policy
that no longer validates (a root removed while serving) fails operations with
`internal_error`, since it is not the request's fault (`hostfs.ErrInvalidPolicy`).

Tests: `TestPathRefusalsAreForbiddenForEveryCaller` and
`TestUnusablePathPoliciesAreInternalErrors` (platform),
`TestServerReadsNoHostFile` and `TestMCPServeReadsNoHostFileOrEnvironment`
(a team server's policy over HTTP, with MCP mounted), `TestComposedManifestsBuildTheSourcesComponents`
(a local repository refused) and `TestRefusalsNameTheRoots` (hostfs).
