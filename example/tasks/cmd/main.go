// Command cmd serves the reports example with tasks enabled over Streamable
// HTTP on a local port and runs the walkthrough against it. It then calls the
// tool as a client that did not declare the extension.
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
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
	example "github.com/eberle1080/mcp/example/tasks"
)

func main() {
	ctx := context.Background()
	srv, err := example.NewServer(200 * time.Millisecond)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = http.Serve(listener, srv.HTTP(ctx, listener.Addr().String()).Handler) }()
	options := func(declare bool) *mcp.ClientOptions {
		o := &mcp.ClientOptions{
			Name: "reports-host", Version: "1.0", ProtocolVersion: schema.LatestProtocolVersion,
			Transport: mcp.ClientTransport{Type: "streamable", ClientTransportHTTP: mcp.ClientTransportHTTP{URL: "http://" + listener.Addr().String() + "/mcp"}},
		}
		if declare {
			o.Extensions = map[string]map[string]interface{}{tasks.ExtensionID: {}}
		}
		return o
	}

	fmt.Println("== a client that declared the tasks extension")
	cli, err := mcp.NewClientWithContext(ctx, example.User{}, options(true))
	if err != nil {
		log.Fatal(err)
	}
	if err := example.Walkthrough(ctx, cli, os.Stdout); err != nil {
		log.Fatal(err)
	}
	cli.Close()

	fmt.Println("== a client that did not")
	plain, err := mcp.NewClientWithContext(ctx, example.User{}, options(false))
	if err != nil {
		log.Fatal(err)
	}
	defer plain.Close()
	result, err := plain.CallTool(ctx, &schema.CallToolRequestParams{Name: "build_report", Arguments: map[string]interface{}{"quarter": "2026-Q3"}})
	if err != nil {
		log.Fatal(err)
	}
	raw, _ := result.Content[0].(map[string]interface{})
	fmt.Printf("CallTool: %v\n", raw["text"])
}
