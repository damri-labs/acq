# Optional local MCP SDK regression

Maintained by Instinct, acting on behalf of Tal Chaim Damri.

This opt-in developer test compiles a Go test fixture from this checkout and
connects two real TypeScript SDK clients to its loopback HTTP server. It protects
SDK handshake,
capability/result-schema and tool-error behavior that the Go handler tests and
handwritten JSON-RPC examples do not exercise through an SDK.

Requirements: Go on PATH, Node 22 or later, npm. No model, user account, hosted
room, desktop application or GUI is used. This does not certify MCP conformance,
client application support, model behavior, or external adoption.

```sh
cd test/mcp-sdk
npm ci --ignore-scripts
npm test
```

The exact SDK version is pinned in package.json and the dependency tree is pinned
in package-lock.json. Install is a separate explicit step and needs registry
access. The test itself makes no intentional external requests: it chooses a
loopback listener with an OS-assigned port, compiles with a 60-second build limit,
creates its own synthetic room, and validates the fixture URL as HTTP on
127.0.0.1 before connecting. There is no endpoint flag or environment override
that can redirect SDK requests to production. The Go fixture also checks the
bound listener address is loopback before signaling readiness. The short-lived
room contains made-up text only.

The fixture reuses the existing Go handler-test server (`newTestServer`), not
production `main()`. Real MCP and room handlers run over HTTP, but this test
does not exercise production startup, persisted-state loading, janitor, shutdown
draining or the outer request-size wrapper. Production listener behavior is
unchanged. The opt-in fixture is skipped in the normal Go test suite; the Node
harness selects it explicitly and isolates all test temporary directories.

Checks:
- Both independent SDK clients negotiate and list the nine current tools.
- Create/join and A-to-B-to-A synthetic text exchange.
- Duplicate cid returns the original sequence and only one delivered message.
- Wrong token is an isError tool result containing HTTP 404.
- End rejects a subsequent send with HTTP 410, while ended history stays readable.

Each SDK request has a 3-second timeout; the entire test has a 90-second limit.
Temporary storage and the server/profile environment are isolated from production.
In normal success or test failure, clients close, the process receives SIGTERM
(with SIGKILL fallback after 3 seconds), and its temporary storage is deleted.
A killed test runner or machine failure can interrupt cleanup; no cleanup promise
is made for that case. The test does not print the room ID, tokens or transcript.
The Go listener reserves its own port and stays bound until shutdown; there is
no separate find-free-port/rebind step.

This is intentionally not the MCPFlo GUI exercise or an import of MCPFlo's test
harness. It keeps ACQ runtime and Go dependencies unchanged. Dependency audit and
SDK updates remain maintenance work: update deliberately, rerun this test and
review changed negotiation/validation behavior rather than silently floating to
latest.
