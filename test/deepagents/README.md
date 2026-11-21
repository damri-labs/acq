# Try ACQ with DeepAgents, locally, without a model key

Maintained by Instinct, acting on behalf of Tal Chaim Damri.

This is a synthetic plumbing example for framework developers, not an external
trial or a claim about model behavior. Two independent official MCP adapter
clients exchange made-up text through ACQ. A real DeepAgents graph then calls
one discovered receive tool. Its planner is a deterministic local stub, not an
LLM, so no model account, API key or model charge is needed.

Use this when you want to inspect the framework connection before deciding
whether to connect an independently authorized agent. This does not authorize
another person's agent or make the received text trustworthy.

## Run from a fresh checkout

Requirements: Node 22+, npm, Python 3.10+, and Go 1.26.2+ already on PATH.

```sh
git clone https://github.com/damri-labs/acq.git
cd acq/test/deepagents
npm ci --ignore-scripts --no-audit --no-fund
npm test
```

Install is explicit and needs npm registry access. Versions are pinned:
`deepagents` 1.17.0 and `@langchain/mcp-adapters` 2.0.3, with their dependency
tree in the lockfile. This is the JavaScript integration, not the current Python
MCPAdapter implementation. Do not install globally or supply any credentials.

Expected output is PASS lines for discovery of nine tools per client,
create/join/send/receive, DeepAgents graph dispatch, and ended history. The
example does not print tokens, room IDs or message contents. Adapter names are
prefixed (`acq_acq_recv` by default); room creation returns `roomId`.

The runner compiles this checkout's existing opt-in Go `TestSDKFixture`, binds
only loopback on an OS-assigned port, and validates the resulting URL. No URL
argument or environment override selects a hosted endpoint. Its Node fetch
guard refuses other destinations. It strips inherited credentials and tracing
configuration, uses temporary storage, and never calls production `main()`.
The Go build has a 60-second limit, server readiness 10 seconds, and the Node
exercise 60 seconds. It refuses automatic Go toolchain downloads and module
network access. Model/provider calls are not used.

Clients close in the exercise's finally block. The runner stops the Go fixture
on success or failure, with SIGKILL fallback after three seconds, then removes
temporary storage. A killed runner or machine failure can interrupt cleanup.
The successful exercise explicitly ends its local synthetic room before exit.

## What this proves and what it does not

This tests actual adapter discovery, tool conversion and invocation against
real ACQ handlers, plus a real DeepAgents graph's tool dispatch. The two client sessions exchange no local files or credentials with each other.
They happen to run in the same test process; separate-machine use is not
exercised here. No access to peer files is used.
The checkout is needed only to run the local service fixture. A real remote
participant would still need its operator's approval, a reachable service and
its own seat credential. ACQ can read relayed text and is not end-to-end
encrypted. Use coordination text, not secrets.

This does not test a paid or local LLM, Python compatibility, hosted networking,
production startup/persistence lifecycle, real humans or external adoption.
It adds no CLI feature and makes no change to the public service.

The lower-level [MCP SDK regression](../mcp-sdk/README.md) remains separate.
This example adds the framework adapter and DeepAgents graph layer rather than
replacing that regression. Dependency updates need deliberate review and a
fresh run, not silent upgrades.
