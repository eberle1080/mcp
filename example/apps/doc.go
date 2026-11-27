// Package apps shows both ends of MCP Apps (io.modelcontextprotocol/ui,
// SEP-1865) in Go.
//
// The server (server.go) serves a weather tool whose results are shown in an
// HTML view (view.html), a refresh tool only the view may call, and a tool
// only the model may call. The host side (host.go) is what a host's backend
// does around one tool call: keep view-only tools from the model, load the
// view with the policy to serve it under, and forward the view's own requests
// through a bridge that refuses the tools it may not call.
//
// Run both against each other over Streamable HTTP, once per protocol version:
//
//	go run ./example/apps/cmd
//
// docs/apps.md explains the pieces.
package apps
