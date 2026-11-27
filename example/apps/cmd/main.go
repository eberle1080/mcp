// Command cmd serves the weather app over Streamable HTTP on a local port and
// runs the host walkthrough against it, once per protocol version. It then
// lists the tools as a client that renders no views sees them.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/eberle1080/mcp"
	"github.com/eberle1080/mcp-protocol/extension/apps"
	example "github.com/eberle1080/mcp/example/apps"
)

func main() {
	ctx := context.Background()
	srv, err := example.NewServer(time.Now)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = http.Serve(listener, srv.HTTP(ctx, listener.Addr().String()).Handler) }()
	url := "http://" + listener.Addr().String() + "/mcp"

	connect := func(version string, rendersViews bool) *mcp.ClientOptions {
		options := &mcp.ClientOptions{
			Name:            "weather-host",
			Version:         "1.0",
			ProtocolVersion: version,
			Transport:       mcp.ClientTransport{Type: "streamable", ClientTransportHTTP: mcp.ClientTransportHTTP{URL: url}},
		}
		if rendersViews {
			options.Extensions = map[string]map[string]interface{}{apps.ExtensionID: apps.ClientSettings()}
		}
		return options
	}
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		fmt.Printf("== MCP %s, a host that renders views\n", version)
		cli, err := mcp.NewClientWithContext(ctx, example.Host{}, connect(version, true))
		if err != nil {
			log.Fatal(err)
		}
		err = example.Walkthrough(ctx, cli, os.Stdout)
		cli.Close()
		if err != nil {
			log.Fatal(err)
		}
	}

	fmt.Println("== MCP 2026-07-28, a client that renders no views")
	cli, err := mcp.NewClientWithContext(ctx, example.Host{}, connect("2026-07-28", false))
	if err != nil {
		log.Fatal(err)
	}
	defer cli.Close()
	listed, err := cli.ListTools(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	fmt.Printf("server tools: %s\n", strings.Join(names, ", "))
}
