# Usability fixtures

`lab-config.yaml` is a lab Collector configuration, copied byte for byte.
An agent using the MCP server could not validate it without editing it: a
`${file:/run/lab/authorization.yaml}` header (a container path absent on this
host) and the `health_check` extension. It must validate and run unmodified.
Never edit it to make a test pass.
