package main

// Version identifies the runtime artifact (the server code), reported as
// serverInfo.version in the MCP initialize handshake. It is deliberately
// independent of the MCP registry's manifest version: registry publications
// are immutable, so a metadata-only republication bumps the registry version
// without any runtime change. The registry's latest advertised version can
// therefore legitimately differ from the served artifact's version between
// runtime releases; bump this constant when the runtime itself changes.
const Version = "1.1.0"
