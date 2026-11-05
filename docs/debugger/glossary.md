# Glossary

The words the web client, CLI and MCP show users. Use these words with these
meanings, and nothing else. If a screen needs a word that isn't listed here, add
it here first. Code identifiers can keep their own names (for example
`draft` in `web/src/workspace/`), but those names never show up in the
interface.

## The things you work with

| Word | Meaning |
| --- | --- |
| **Configuration** | Your Collector YAML. The debugger never changes it. |
| **Scenario** | The YAML file that says what to run the configuration on: inputs, mocks, assertions and settings. It's separate from the configuration. |
| **Input** | Telemetry sent to a receiver during a run: inline records, a file or a capture. |
| **Mock** | Something that stands in for part of the environment during a run, such as files, Kubernetes objects, environment variables or providers. |
| **Assertion** | An expected result, checked after the run. The outcome is Pass, Fail, Inconclusive or Error. |
| **Test** | A scenario with at least one assertion. It isn't a separate object: adding an assertion makes a scenario a test. |
| **Template** | A small configuration that runs as it is (OTLP receiver → debug exporter). **New** starts from it. |

## What you do

| Word | Meaning |
| --- | --- |
| **New** | Start from the template configuration and an empty scenario. |
| **Open** | Load a configuration or scenario from a file, a paste, an example or saved work. The original is never changed: you edit a copy. |
| **Run** | Run the configuration on the scenario. This is always the action's label, whether the scenario has one input or ten. |
| **Add assertions** | Take assertions the run suggests, after confirming each one, into the open scenario. Nothing is saved. |
| **Save** | Store the open configuration or scenario in saved work as a new revision. This is the only action that saves. |
| **Export for CI** | The one download: a zip with the configuration, its scenarios, a GitHub Actions workflow and the `linea test` command. It saves what is open first. From a run, it opens the run's configuration and scenario first. Unpack it at the repository root. See [ci.md](ci.md). |
| **Import** | Load an edited Export for CI zip back. Each changed file becomes a new revision of the item it came from. |
| **Compare** | Put two runs side by side. |
| **Capture** | Record telemetry from a live Collector so it can be used as input. |

## Where things live

| Word | Meaning |
| --- | --- |
| **Saved work** | Configurations, scenarios and fixtures stored by the server: in its PostgreSQL database and object storage (every `linea-server` is a team server). |
| **Revision** | One saved version of a saved item. Saving adds a revision, and earlier revisions stay unchanged. |
| **Fixture** | A saved file that a scenario refers to, such as input records or a mocked file. |
| **Run** (noun) | The record of one run: its inputs, every stage, the result and the assertion outcomes. It never changes afterwards, and its 8-character ID identifies it. |
| **Suite** | Several saved scenarios run against one configuration. Its outcome is the **test result**. |
| **Runtime** | The Collector build a run executes on: built in, a local container or a worker connected to a team server. |
| **Project** | One team's space on a team server: its runs, suites, saved work and captures. Requests that name none work in **Default**. |
| **Library** | The built-in project where an admin publishes configurations, fixtures and scenarios and shares them with every project or some. |
| **Share** | Choose the projects a library item or a runtime is available to. |

## Save states

The header shows the save state of what is open, for both the configuration and
the scenario:

| State | When |
| --- | --- |
| **Not saved** | It isn't in saved work: it's new, or it was opened from a file or an example. |
| **Saved · revision N** | It matches revision N in saved work. |
| **Saved · earlier revision** | It matches a revision that a newer one has replaced. |
| **Unsaved changes** | It's in saved work, but it was edited after it was opened or saved. |
