# Outbound

Outbound is a reverse tunnel written in Go. Agents running behind firewalls
open a persistent bidirectional gRPC stream to the edge server. The edge
forwards inbound HTTP requests through that tunnel and streams the responses
back to callers.

## Project structure

```
cmd/agent/      Agent binary entrypoint (--id, --token, --service, --edge, --insecure)
cmd/edge/       Edge binary entrypoint (--addr, --auth-secret, --request-timeout,
                --keepalive-interval, --keepalive-timeout, --shutdown-timeout)
cmd/demo/       Demo upstream HTTP server
internal/agent/ Agent client: register, proxy HTTP upstream, reconnect loop
internal/config/ ServiceFlag: repeatable --service name=port flag parser
internal/edge/  Edge server: session management, auth, HTTP dispatch
internal/tunnel Shared constants (headers, request ID generation, header helpers)
proto/          Protobuf definitions (TunnelService, RegisterRequest, etc.)
internal/tunneltest/ In-process test harness using bufconn and httptest.Server
```

## Architecture decisions

**Single-port multiplexing.** The edge uses cmux to serve HTTP and gRPC on the
same TCP listener (default `:8080`). Incoming connections are routed by
content-type: frames with `application/grpc` go to the gRPC server; everything
else goes to the HTTP server. Both servers shut down gracefully on SIGINT or
SIGTERM within `--shutdown-timeout` (default 30 s).

**Auth.** Single-tenant pre-shared secret. The agent sends the token in
RegisterRequest.Token (proto field 3). The edge validates it with
subtle.ConstantTimeCompare. On rejection the edge sends RegisterAck{ok:false,
message:"unauthorized"} and returns nil (clean close). The agent enters
select{} permanently on rejection and does not retry.

**Agent ID.** If `--id` is omitted the agent generates a random 8-byte hex ID
at startup and logs it. The ID is stable for the lifetime of the process.

**Reconnect.** The agent reconnect loop uses exponential backoff with jitter.
agent.NewClient is constructed once outside the loop so the http.Transport
connection pool is reused across reconnects.

**Keepalive.** The edge sends Ping frames on a configurable interval and expects
a Pong within KeepaliveTimeout. A missing Pong drops the session.

**Session lifecycle.** Register attaches a session keyed by agentID. A duplicate
RegisterRequest on an established session is ignored. detachSession is only
called on a non-rejection error path, not on clean auth rejection.

**Dispatch.** The edge Recv goroutine is isolated so keepalive and dispatch
never block on a slow stream. Response dispatch is non-blocking; buffer overflow
fails the pending response immediately rather than blocking.

**Buffers.** 64 KB read buffers are pooled via sync.Pool in both the agent and
the edge to reduce GC pressure.

**Request body limit.** The edge rejects request bodies larger than 10 MB
(hardcoded `maxRequestBody`). The limit is enforced both on `Content-Length`
and while streaming chunks. When the limit is exceeded mid-stream the edge
sends an Error frame to the agent before returning 413 to the caller.

## Conventions

Commit messages use `type: subject` on the first line followed by a blank line
and a prose body written in full sentences. Do not use bullet points or dashes
in commit messages.

Keep changes minimal and focused. Do not introduce new dependencies without
discussion. Do not widen test assertions to accept bad product behaviour; flag
the unexpected behaviour instead.

The test harness runs the edge gRPC server in-process over bufconn and the
edge HTTP server via httptest.Server; they share the same edge.Server instance
but listen on separate in-process transports. Tests that expect a rejected
agent must set SkipInitialReady: true in HarnessOptions. Timing-based
assertions (time.Sleep) are not acceptable; use a deterministic polling loop
with a fixed deadline instead.

Only comment code that involves non-obvious concurrency decisions. Remove
explanatory comments that restate what the code already clearly says.
