// Command cmd serves the example skills over Streamable HTTP on a local port
// and runs the host walkthrough against them, once per protocol version.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/eberle1080/mcp"
	"github.com/eberle1080/mcp/example/skills"
)

func main() {
	ctx := context.Background()
	compiled, err := skills.Compile(ctx)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := skills.NewServer(compiled, time.Now)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = http.Serve(listener, srv.HTTP(ctx, listener.Addr().String()).Handler) }()
	url := "http://" + listener.Addr().String() + "/mcp"

	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		fmt.Printf("== MCP %s\n", version)
		cli, err := mcp.NewClientWithContext(ctx, skills.Host{}, &mcp.ClientOptions{
			Name:            "skills-host",
			Version:         "1.0",
			ProtocolVersion: version,
			Transport:       mcp.ClientTransport{Type: "streamable", ClientTransportHTTP: mcp.ClientTransportHTTP{URL: url}},
		})
		if err != nil {
			log.Fatal(err)
		}
		err = skills.Walkthrough(ctx, cli, os.Stdout)
		cli.Close()
		if err != nil {
			log.Fatal(err)
		}
	}
}
