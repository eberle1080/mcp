package apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/eberle1080/jsonrpc"
	pclient "github.com/eberle1080/mcp-protocol/client"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/eberle1080/mcp/client"
	"github.com/eberle1080/mcp/client/apphost"
)

// Walkthrough does, on the server side of a host that renders MCP Apps, what
// the host does around one tool call, and writes what it sees to out:
//
//  1. Lists the tools and keeps the view-only ones from the model.
//  2. The model calls get_weather.
//  3. Loads the tool's view, checked, with the policy to serve it under. A
//     host's front end would now load HTML into a sandboxed iframe on another
//     origin, send it the tool's result, and route its messages to a Bridge.
//  4. The view, through a Bridge, refreshes itself, reads its own resource,
//     and is refused a model-only tool.
func Walkthrough(ctx context.Context, cli *client.Client, out io.Writer) error {
	listed, err := cli.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "server tools: %s\n", names(listed.Tools))
	fmt.Fprintf(out, "model tools: %s\n", names(apphost.ModelTools(listed.Tools)))

	result, err := cli.CallTool(ctx, &schema.CallToolRequestParams{Name: "get_weather", Arguments: map[string]interface{}{"city": "Lisbon"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "model called get_weather: %v\n", textOf(result))

	var tool schema.Tool
	for _, t := range listed.Tools {
		if t.Name == "get_weather" {
			tool = t
		}
	}
	resources, err := cli.ListResources(ctx, nil)
	if err != nil {
		return err
	}
	var listing *schema.Resource
	for i := range resources.Resources {
		if resources.Resources[i].Uri == DashboardURI {
			listing = &resources.Resources[i]
		}
	}
	view, err := apphost.LoadView(ctx, cli, tool, listing)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "view %s: %d bytes, %s\n", view.URI, len(view.HTML), view.Digest)
	fmt.Fprintf(out, "serve it with Content-Security-Policy: %s\n", view.CSP)
	fmt.Fprintf(out, "iframe allow: %q, border: %v\n", view.Allow, view.Meta.PrefersBorder != nil && *view.Meta.PrefersBorder)

	bridge := apphost.NewBridge(cli, listed.Tools)
	bridge.Audit = func(_ context.Context, method, name string, refused error) {
		verdict := "forwarded"
		if refused != nil {
			verdict = "refused"
		}
		fmt.Fprintf(out, "audit: view %s %s %s\n", verdict, method, name)
	}
	refreshed, err := bridge.CallTool(ctx, &schema.CallToolRequestParams{Name: "refresh_weather", Arguments: map[string]interface{}{"city": "Lisbon"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "view refreshed: %v\n", refreshed.StructuredContent)
	if _, err := bridge.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: DashboardURI}); err != nil {
		return err
	}
	_, err = bridge.CallTool(ctx, &schema.CallToolRequestParams{Name: "explain_weather", Arguments: map[string]interface{}{"city": "Lisbon"}})
	if !errors.Is(err, apphost.ErrRefused) {
		return fmt.Errorf("a model-only tool was not refused to the view: %v", err)
	}
	fmt.Fprintf(out, "refused: %v\n", err)
	return nil
}

func names(tools []schema.Tool) string {
	var result []string
	for _, tool := range tools {
		result = append(result, tool.Name)
	}
	sort.Strings(result)
	return strings.Join(result, ", ")
}

func textOf(result *schema.CallToolResult) string {
	var parts []string
	for _, item := range result.Content {
		switch content := item.(type) {
		case schema.TextContent:
			parts = append(parts, content.Text)
		case map[string]interface{}:
			if text, ok := content["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, " ")
}

// Host is a client handler that serves no requests of its own.
type Host struct{}

var _ pclient.Handler = Host{}

func (Host) Notify(context.Context, *jsonrpc.Notification) error   { return nil }
func (Host) NextRequestID() jsonrpc.RequestId                      { return 0 }
func (Host) LastRequestID() jsonrpc.RequestId                      { return 0 }
func (Host) Implements(string) bool                                { return false }
func (Host) Init(context.Context, *schema.ClientCapabilities)      {}
func (Host) OnNotification(context.Context, *jsonrpc.Notification) {}
func (Host) ListRoots(context.Context, *jsonrpc.TypedRequest[*schema.ListRootsRequest]) (*schema.ListRootsResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("roots/list", nil)
}
func (Host) CreateMessage(context.Context, *jsonrpc.TypedRequest[*schema.CreateMessageRequest]) (*schema.CreateMessageResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("sampling/createMessage", nil)
}
func (Host) Elicit(context.Context, *jsonrpc.TypedRequest[*schema.ElicitRequest]) (*schema.ElicitResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("elicitation/create", nil)
}
