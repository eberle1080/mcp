# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common Development Commands

### Testing

```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run a specific test
go test -v ./server -run TestServerAsClient

# Run tests in a specific package
go test -v ./client
```

### Building

```bash
# Build the bridge binary
cd bridge && go build -o mcpb .

# Run the bridge directly
go run ./bridge -h

# Cross-compile the bridge for multiple platforms (uses build.yaml)
cd bridge
export GOOS=linux && export GOARCH=amd64 && go build -o mcpb .
export GOOS=darwin && export GOARCH=arm64 && go build -o mcpb .
```

### Running Examples

```bash
# Run example servers
go run ./example/fs           # File system example
go run ./example/tool         # Tool example
go run ./example/resource     # Resource example

# Run auth examples
go run ./example/auth/term         # Terminal auth example
go run ./example/auth/experimental # Experimental auth example
go run ./example/auth/percall      # Per-call auth example
```

### Dependencies

```bash
# Update dependencies
go get -u ./...
go mod tidy

# Vendor dependencies (if needed)
go mod vendor
```

## Architecture Overview

This is a Go implementation of the Model Context Protocol (MCP) - a standardized JSON-RPC-based protocol for AI model-application communication. The codebase is split between **protocol definitions** (in the `mcp-protocol` dependency) and **transport/server implementations** (in this repo).

### Core Components

1. **Server Package** (`/server`): MCP server implementation
   - HTTP/SSE transport (default: `/sse` and `/message` endpoints)
   - Streamable HTTP transport (single endpoint: `/mcp`)
   - Stdio transport (for editor integrations)
   - Handler-based architecture: users implement `server.Handler` interface
   - `DefaultHandler` provides no-op stubs for all protocol methods

2. **Client Package** (`/client`): MCP client implementation
   - Connects to MCP servers via SSE, Streamable HTTP, or stdio
   - Supports server-initiated calls (roots, sampling, elicitation)
   - Automatic reconnection and session recovery
   - Background keepalive pinger

3. **Bridge Package** (`/bridge`): Proxy binary (`mcpb`)
   - Forwards JSON-RPC between local (stdio/HTTP) and remote (HTTP/SSE) endpoints
   - Transparently handles OAuth2/OIDC authentication
   - Use case: Connect desktop/CLI apps to remote MCP servers

4. **Auth Packages**:
   - `server/auth`: Server-side OAuth2/OIDC enforcement (global + per-tool/resource)
   - `client/auth`: Client-side OAuth2 token fetching with automatic retry on 401
   - Supports RFC 9728 protected resource metadata discovery
   - Backend-for-Frontend (BFF) flow support

### Protocol Layer Architecture

```
Application (Custom Handler)
         ↓
server.Handler Interface (from mcp-protocol package)
         ↓
Transport Layer (HTTP/SSE, Streamable HTTP, or Stdio)
         ↓
JSON-RPC (viant/jsonrpc)
         ↓
Network
```

### Key Design Patterns

- **Handler Factory Pattern**: Servers use `NewHandler` functions that return `server.Handler` implementations
- **Transport Abstraction**: Same handler works over HTTP, SSE, Streamable, or stdio
- **Adapter Pattern**: `Adapter` converts server handlers to client interfaces for testing
- **Middleware Chain**: HTTP requests flow through CORS → Auth → Protocol validation → Transport handler
- **Registry Pattern**: Resources, tools, and prompts are registered with handlers via registries

### Handler Implementation

To create a custom MCP server:

1. Embed `serverproto.DefaultHandler` (from mcp-protocol package)
2. Override methods like `ListResources`, `ReadResource`, `ListTools`, `CallTool`
3. Use `RegisterResource()` and `RegisterTool()` helpers for simple cases
4. Implement `Implements(method)` to declare supported protocol methods

**Quick pattern for inline handlers**:

```go
newHandler := serverproto.WithDefaultHandler(ctx, func(h *serverproto.DefaultHandler) error {
    h.RegisterResource(schema.Resource{Name: "foo", Uri: "/foo"},
        func(ctx context.Context, req *schema.ReadResourceRequest) (*schema.ReadResourceResult, *jsonrpc.Error) {
            return &schema.ReadResourceResult{Contents: []schema.ReadResourceResultContentsElem{{Text: "bar"}}}, nil
        })
    return nil
})
```

### Transport Selection

**Server-side**:
- `srv.HTTP(ctx, ":4981")` - SSE transport by default
- `srv.UseStreamableHTTP(true)` then `srv.HTTP(...)` - Streamable HTTP
- `srv.Stdio(ctx)` - stdio transport

**Client-side** (in `/client.go` factory):
- Auto-detects based on `ClientOptions.Transport.Type` (SSE, Streamable, or Stdio)
- Builds appropriate transport with optional auth RoundTripper

### Authentication Flow

**Server**:
1. Configure `authorization.Policy` with OAuth2/OIDC settings
2. Create `auth.Service` from policy
3. Wire middleware: `WithAuthorizer(service.Middleware)` and `WithJRPCAuthorizer(service.EnsureAuthorized)`
4. Exposes `/.well-known/oauth-protected-resource` metadata

**Client**:
1. Create auth `RoundTripper` with token store and auth flow (e.g., browser PKCE)
2. Pass custom `http.Client` to transport (SSE or Streamable)
3. On 401 response:
   - Discover protected resource and auth server metadata
   - Acquire OAuth2 token via configured flow
   - Retry request with `Authorization: Bearer <token>`

### Protocol Methods

**Server implements** (called by clients):
- `initialize`, `ping`
- `resources/list`, `resources/read`, `resources/subscribe`, `resources/unsubscribe`
- `prompts/list`, `prompts/get`
- `tools/list`, `tools/call`
- `complete`, `logging/setLevel`

**Client implements** (called by servers):
- `roots/list`
- `sampling/createMessage` (LLM sampling via client)
- `elicitation/create` (user prompt via client)
- `interaction/create` (user interaction)

## Important Patterns and Conventions

### Schema Types

The `schema` package (from mcp-protocol) defines all protocol types:
- `schema.Implementation{Name: "server-name", Version: "1.0"}` - use struct literal with **named fields**
- `schema.Resource`, `schema.Tool`, `schema.Prompt` - resource types
- `schema.ReadResourceRequest`, `schema.CallToolRequest` - request types
- Always use pointer receivers for handlers: `func (h *MyHandler) ListTools(...)`

### Error Handling

- Return `*jsonrpc.Error` (not Go `error`) from handler methods
- Use `jsonrpc.NewInternalError(msg, data)` for server errors
- Use `jsonrpc.NewInvalidParamsError(msg)` for bad input
- Check `schema.Unauthorized` error code in clients to trigger auth retry

### Context and Cancellation

- Always pass `context.Context` through request chains
- Handlers receive context from transport layer
- Use `context.WithCancel` for long-running operations
- Protocol supports explicit cancellation via `cancelled` method

### Notifications and Subscriptions

- Use `handler.Notifier` to send notifications to clients
- `resources/updated` - notify clients when resources change
- `tools/list_changed` - notify when tool list changes
- Subscriptions track which resources clients are watching

## Common Gotchas

1. **Schema struct literals**: Use named fields when creating `schema.Implementation`:
   ```go
   // CORRECT
   schema.Implementation{Name: "server", Version: "1.0"}

   // WRONG (will fail compilation)
   schema.Implementation{"server", "1.0"}
   ```

2. **Handler embedding**: Always embed `*serverproto.DefaultHandler` (pointer, not value):
   ```go
   type MyHandler struct {
       *serverproto.DefaultHandler  // Correct
   }
   ```

3. **Transport endpoint paths**:
   - SSE default: `GET /sse` (stream) + `POST /message` (send)
   - Streamable default: `POST /mcp` (bidirectional)
   - Customize via `WithSSEURI()`, `WithStreamableURI()`

4. **Auth middleware ordering**: Auth middleware must come before protocol handlers in the chain

5. **Client initialization**: Always call `client.Initialize(ctx)` before other operations

6. **Stdio transport**: When using stdio, the parent process must properly handle child process lifecycle

## Testing

- Use `Adapter` to test server handlers as clients without network transport
- See `server/adapter_test.go` and `client/local_test.go` for examples
- Auth examples in `example/auth/` demonstrate different auth patterns
- Mock implementations available in `client/auth/mock/`

## Documentation References

- **Server Implementation**: `/docs/implementer.md` - detailed handler guide
- **Server Features**: `/docs/server_guide.md` - resources, tools, prompts
- **Authentication**: `/docs/authentication.md` - OAuth2/OIDC setup
- **Bridge Usage**: `/docs/bridge.md` - proxy/bridge binary guide
- **Client Usage**: `/docs/client.md` - client API guide
- **Official MCP Spec**: https://modelcontextprotocol.io/introduction

## Module and Dependencies

- **Module**: `github.com/eberle1080/mcp` (this is a fork; upstream is `github.com/viant/mcp`)
- **Protocol Package**: `github.com/eberle1080/mcp-protocol` (fork of `github.com/viant/mcp-protocol`)
- **JSON-RPC**: `github.com/eberle1080/jsonrpc` - transport abstraction
- **OAuth2**: `golang.org/x/oauth2` + `github.com/viant/scy` - auth flows
- **File System**: `github.com/viant/afs` - abstract file system for examples

Go version: 1.23.8 (see `go.mod`)
