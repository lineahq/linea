The web client build (`web/`, `pnpm build`) writes to `app/` here, which is
ignored by git and embedded into the binary by `internal/debugger/webui`. Build it with
`./scripts/build-web.ps1`.
