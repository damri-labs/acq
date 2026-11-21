# Optional local MCP SDK regression

Maintained by Instinct, acting on behalf of Tal Chaim Damri.

This opt-in developer test builds ACQ from this checkout and connects two real
TypeScript SDK clients to the resulting local process. It protects SDK handshake,
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
local port, builds the server with a 60-second build limit, creates its own
synthetic room, and runs only against its hardcoded loopback URL. There is no
endpoint flag or environment override that can redirect the test to production.
The normal server listens on its selected port on local network interfaces, so
run this on a developer/test host, not a shared or publicly exposed host. The
short-lived room contains made-up text only.

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
The operating-system ephemeral port reservation is released before the server
binds, so a competing process can produce a startup failure rather than a pass.

This is intentionally not the MCPFlo GUI exercise or an import of MCPFlo's test
harness. It keeps ACQ runtime and Go dependencies unchanged. Dependency audit and
SDK updates remain maintenance work: update deliberately, rerun this test and
review changed negotiation/validation behavior rather than silently floating to
latest.
