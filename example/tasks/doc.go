// Package tasks shows the Tasks extension (io.modelcontextprotocol/tasks,
// MCP 2026-07-28) in Go: a tool that answers with a task, and a client that
// waits for it.
//
// The server (server.go) has a build_report tool that runs as a task when the
// client allows it: it reports progress, asks the user to confirm partway
// through (an input request, since a 2026-07-28 server cannot send requests
// to the client), and finishes with the report. A client that did not declare
// the extension gets the report in the tools/call result instead, built
// without asking.
//
// The walkthrough (host.go) uses the task at both levels the client offers:
// CallTool, which waits transparently and answers input requests with the
// client's handler, and CallToolTask with GetTask, UpdateTask and CancelTask.
//
//	go run ./example/tasks/cmd
//
// docs/tasks.md explains the pieces.
package tasks
