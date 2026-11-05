package api

func artifactFailure(description string) map[string]any {
	return reply(description, ref("ArtifactError"))
}

func artifactCollectionPath() map[string]any {
	return map[string]any{
		"post": op("saveArtifact", "Save original source bytes as a new revision and atomically advance its head. Requires a server that keeps saved work; does not validate or execute the source.", nil, "SaveRequest", map[string]any{
			"201": reply("The saved revision and current artifact metadata.", ref("Saved")),
			"400": artifactFailure("Malformed request, invalid kind, missing edit expectations or attempted kind change."),
			"404": artifactFailure("Unknown artifact to update."),
			"409": artifactFailure("Wrong workspace or stale edit; current metadata is supplied for a stale head."),
			"413": artifactFailure("Source, catalog, revision count or physical storage limit exceeded."),
			"500": artifactFailure("Storage or integrity failure; no successful save is acknowledged."),
			"503": artifactFailure("The server keeps no saved work or admission is closed."),
		}),
		"get": op("listArtifacts", "List saved source-artifact metadata. Continuation conflicts if the catalog changes.", []any{
			map[string]any{"name": "kind", "in": "query", "schema": map[string]any{"type": "string"}},
			map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 25}},
			map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}},
		}, "", map[string]any{
			"200": reply("A bounded metadata page and workspace identity.", ref("Page")),
			"400": artifactFailure("Invalid filter, limit or cursor."),
			"409": artifactFailure("Catalog changed; restart listing."),
			"500": artifactFailure("Storage unavailable."),
			"503": artifactFailure("The server keeps no saved work or admission is closed."),
		}),
	}
}

func artifactParams(revision bool) []any {
	params := []any{map[string]any{"name": "artifact", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}
	if revision {
		params = append(params, map[string]any{"name": "revision", "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
	}
	return params
}

func artifactMetadataPath() map[string]any {
	return map[string]any{"get": op("getArtifact", "Current metadata and immutable head revision identity.", artifactParams(false), "", map[string]any{
		"200": reply("Artifact metadata.", ref("Metadata")),
		"404": artifactFailure("Unknown artifact."),
		"500": artifactFailure("Storage unavailable."),
		"503": artifactFailure("The server keeps no saved work or admission is closed."),
	})}
}

func artifactRevisionPath() map[string]any {
	return map[string]any{"get": op("getArtifactRevision", "Exact original source bytes (base64) and revision metadata; source can contain sensitive values.", artifactParams(true), "", map[string]any{
		"200": reply("Immutable revision and original source bytes.", ref("RevisionData")),
		"404": artifactFailure("Unknown artifact/revision reference."),
		"500": artifactFailure("Referenced content is unavailable or corrupt, or storage is unavailable."),
		"503": artifactFailure("The server keeps no saved work or admission is closed."),
	})}
}

func artifactRevisionsPath() map[string]any {
	return map[string]any{"get": op("listArtifactRevisions", "An artifact's revision metadata newest first, in pages that stay stable while it gains revisions. No content.", append(artifactParams(false),
		map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 25}},
		map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}},
	), "", map[string]any{
		"200": reply("The artifact and one page of its revisions.", ref("RevisionPage")),
		"400": artifactFailure("Invalid limit or cursor."),
		"404": artifactFailure("Unknown artifact."),
		"500": artifactFailure("Storage unavailable."),
		"503": artifactFailure("The server keeps no saved work or admission is closed."),
	})}
}

func artifactExportPath() map[string]any {
	return map[string]any{"post": op("exportArtifactsForCI", "Export for CI: a zip of a saved configuration and 1-32 scenarios at their head revisions as collector/config.yaml and collector/tests/*.yaml, the examples/ci GitHub Actions workflow and an linea.json manifest of the exported revisions. Saved bytes are written as they are; no environment value is read.", nil, "ExportForCIRequest", map[string]any{
		"200": map[string]any{"description": "The archive, as an attachment named collector-ci.zip.", "content": map[string]any{"application/zip": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}},
		"400": artifactFailure("Malformed request, no configuration, 0 or more than 32 scenarios, a duplicate, or an artifact of the wrong kind."),
		"404": artifactFailure("Unknown artifact."),
		"500": artifactFailure("Storage unavailable."),
		"503": artifactFailure("The server keeps no saved work or admission is closed."),
	})}
}

func artifactImportPath() map[string]any {
	return map[string]any{"post": op("importArtifacts", "Import a CI archive of the export's layout. A file its manifest names as exported from this server becomes a new revision of that artifact, or is unchanged when equal to the head; any other file becomes a new artifact. The workflow is not saved. Everything is checked before the first save.", nil, "ImportRequest", map[string]any{
		"200": reply("What happened to each file, configuration first.", ref("ArtifactImport")),
		"400": artifactFailure("Not a zip, no configuration or scenario, or an unknown, duplicate or oversized entry."),
		"409": artifactFailure("An exported artifact was revised after its export."),
		"413": artifactFailure("The request or workspace storage limit was exceeded."),
		"500": artifactFailure("Storage unavailable."),
		"503": artifactFailure("The server keeps no saved work or admission is closed."),
	})}
}
