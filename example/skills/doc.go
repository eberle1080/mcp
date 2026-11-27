// Package skills shows both ends of the MCP skills extension
// (io.modelcontextprotocol/skills, SEP-2640).
//
// The server side (server.go) serves a skill compiled from files embedded in
// the binary, and one whose content is generated on each read. The host side
// (host.go) lists the skills, loads one, reads and lists its files, and has
// every read checked by the skillhost package.
//
// Run both against each other over Streamable HTTP, once per protocol version:
//
//	go run ./example/skills/cmd
//
// docs/skills.md explains the pieces.
package skills
