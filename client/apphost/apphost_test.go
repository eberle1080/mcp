package apphost_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/jsonrpc/transport"
	"github.com/eberle1080/mcp-protocol/extension/apps"
	"github.com/eberle1080/mcp-protocol/schema"
	protocol "github.com/eberle1080/mcp-protocol/server"
	"github.com/eberle1080/mcp/client"
	"github.com/eberle1080/mcp/client/apphost"
	"github.com/eberle1080/mcp/server"
	"github.com/stretchr/testify/require"
)

const view = "<!DOCTYPE html><html><body><div id=weather></div></body></html>"

func register(h *protocol.DefaultHandler) error {
	camera := &apps.Permission{}
	meta := apps.ResourceMeta{
		CSP:         &apps.CSP{ConnectDomains: []string{"https://api.openweathermap.org"}, ResourceDomains: []string{"https://cdn.jsdelivr.net"}},
		Permissions: &apps.Permissions{Camera: camera},
	}
	if err := h.RegisterAppResource(schema.Resource{Uri: "ui://weather/dashboard", Name: "weather_dashboard"}, meta, protocol.StaticHTML(view)); err != nil {
		return err
	}
	reply := func(text string) protocol.ToolHandlerFunc {
		return func(context.Context, *schema.CallToolRequest) (*schema.CallToolResult, *jsonrpc.Error) {
			return &schema.CallToolResult{Content: []schema.CallToolResultContentElem{schema.TextContent{Type: "text", Text: text}}}, nil
		}
	}
	h.RegisterToolWithSchema("get_weather", "Current weather", schema.ToolInputSchema{Type: "object"}, nil, reply("Sunny, 72°F"))
	h.RegisterToolWithSchema("refresh_dashboard", "Refresh", schema.ToolInputSchema{Type: "object"}, nil, reply("refreshed"))
	h.RegisterToolWithSchema("explain_weather", "Explain", schema.ToolInputSchema{Type: "object"}, nil, reply("explained"))
	if err := h.LinkToolToApp("get_weather", apps.ToolMeta{ResourceURI: "ui://weather/dashboard"}); err != nil {
		return err
	}
	if err := h.LinkToolToApp("refresh_dashboard", apps.ToolMeta{ResourceURI: "ui://weather/dashboard", Visibility: []apps.Visibility{apps.VisibilityApp}}); err != nil {
		return err
	}
	return h.LinkToolToApp("explain_weather", apps.ToolMeta{Visibility: []apps.Visibility{apps.VisibilityModel}})
}

type wire struct {
	handler transport.Handler
	id      atomic.Uint64
	calls   []string
}

func (w *wire) Notify(ctx context.Context, n *jsonrpc.Notification) error {
	w.handler.OnNotification(ctx, n)
	return nil
}

func (w *wire) Send(ctx context.Context, r *jsonrpc.Request) (*jsonrpc.Response, error) {
	r.Id = w.id.Add(1)
	w.calls = append(w.calls, r.Method)
	response := &jsonrpc.Response{}
	w.handler.Serve(ctx, r, response)
	return response, nil
}

func connect(t *testing.T, version string, declare bool) (*client.Client, *wire) {
	t.Helper()
	ctx := context.Background()
	srv, err := server.New(server.WithNewHandler(protocol.WithDefaultHandler(ctx, register)))
	require.NoError(t, err)
	w := &wire{}
	w.handler = srv.NewHandler(ctx, w)
	options := []client.Option{client.WithProtocolVersion(version)}
	if declare {
		options = append(options, client.WithExtension(apps.ExtensionID, apps.ClientSettings()))
	}
	c := client.New("host", "1", w, options...)
	t.Cleanup(c.Close)
	init, err := c.Initialize(ctx)
	require.NoError(t, err)
	require.Contains(t, init.Capabilities.Extensions, apps.ExtensionID)
	return c, w
}

func names(tools []schema.Tool) []string {
	var result []string
	for _, tool := range tools {
		result = append(result, tool.Name)
	}
	return result
}

func TestTheModelSeesOnlyItsTools(t *testing.T) {
	ctx := context.Background()
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		host, _ := connect(t, version, true)
		listed, err := host.ListTools(ctx, nil)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"get_weather", "refresh_dashboard", "explain_weather"}, names(listed.Tools), version)
		require.ElementsMatch(t, []string{"get_weather", "explain_weather"}, names(apphost.ModelTools(listed.Tools)), version)

		// A client that renders no views never sees the view-only tool.
		plain, _ := connect(t, version, false)
		listed, err = plain.ListTools(ctx, nil)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"get_weather", "explain_weather"}, names(listed.Tools), version)
		// The server reports the unknown tool as a tool error.
		result, err := plain.CallTool(ctx, &schema.CallToolRequestParams{Name: "refresh_dashboard"})
		require.NoError(t, err, version)
		require.True(t, result.IsError != nil && *result.IsError, version)
		require.Contains(t, fmt.Sprint(result.Content[0]), "not found", version)
	}
}

func TestLoadViewChecksTheDocumentAndBuildsItsPolicy(t *testing.T) {
	ctx := context.Background()
	host, _ := connect(t, "2026-07-28", true)
	listed, err := host.ListTools(ctx, nil)
	require.NoError(t, err)
	var weather schema.Tool
	for _, tool := range listed.Tools {
		if tool.Name == "get_weather" {
			weather = tool
		}
	}
	v, err := apphost.LoadView(ctx, host, weather, nil)
	require.NoError(t, err)
	require.Equal(t, "ui://weather/dashboard", v.URI)
	require.Equal(t, view, string(v.HTML))
	require.Equal(t, "camera", v.Allow)
	require.Empty(t, v.RejectedOrigins)
	require.Contains(t, v.CSP, "connect-src 'self' https://api.openweathermap.org;")
	require.Contains(t, v.CSP, "script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net;")
	require.True(t, strings.HasPrefix(v.Digest, "sha256:"))

	_, err = apphost.LoadView(ctx, host, schema.Tool{Name: "plain"}, nil)
	require.Error(t, err)
	_, err = apphost.LoadView(ctx, host, schema.Tool{Name: "bad", Meta: map[string]interface{}{"ui": map[string]interface{}{"resourceUri": "https://example.com/view.html"}}}, nil)
	require.Error(t, err)
}

// serving answers every read with the given result.
type serving struct {
	apphost.Source
	result *schema.ReadResourceResult
}

func (s serving) ReadResource(context.Context, *schema.ReadResourceRequestParams, ...client.RequestOption) (*schema.ReadResourceResult, error) {
	return s.result, nil
}

func TestLoadViewRefusesMalformedReads(t *testing.T) {
	ctx := context.Background()
	tool := schema.Tool{Name: "t", Meta: map[string]interface{}{apps.LegacyResourceURIKey: "ui://x/view"}}
	html := apps.MimeType
	other := "text/html"
	for name, result := range map[string]*schema.ReadResourceResult{
		"no result":    nil,
		"no item":      {},
		"other URI":    {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/other", MimeType: &html, Text: view}}},
		"two items":    {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &html, Text: view}, {Uri: "ui://x/view", MimeType: &html, Text: view}}},
		"plain html":   {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &other, Text: view}}},
		"no type":      {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", Text: view}}},
		"text & blob":  {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &html, Text: view, Blob: base64.StdEncoding.EncodeToString([]byte(view))}}},
		"empty":        {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &html}}},
		"bad base64":   {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &html, Blob: "!!"}}},
		"bad metadata": {Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &html, Text: view, Meta: map[string]interface{}{"ui": "nonsense"}}}},
	} {
		_, err := apphost.LoadView(ctx, serving{result: result}, tool, nil)
		require.Error(t, err, name)
	}

	// A blob works, and the listing's metadata applies when the item has none.
	listing := &schema.Resource{Uri: "ui://x/view", Meta: map[string]interface{}{"ui": map[string]interface{}{"csp": map[string]interface{}{"connectDomains": []interface{}{"https://api.example.com", "https://evil.example; script-src *"}}}}}
	v, err := apphost.LoadView(ctx, serving{result: &schema.ReadResourceResult{Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &html, Blob: base64.StdEncoding.EncodeToString([]byte(view))}}}}, tool, listing)
	require.NoError(t, err)
	require.Equal(t, view, string(v.HTML))
	require.Contains(t, v.CSP, "connect-src 'self' https://api.example.com;")
	require.NotContains(t, v.CSP, "evil")
	require.Equal(t, []string{"https://evil.example; script-src *"}, v.RejectedOrigins)

	// The content item's metadata wins over the listing's, as a whole: this
	// item declares no CSP, so the restrictive default applies.
	v, err = apphost.LoadView(ctx, serving{result: &schema.ReadResourceResult{Contents: []schema.ReadResourceResultContentsElem{{Uri: "ui://x/view", MimeType: &html, Text: view, Meta: map[string]interface{}{"ui": map[string]interface{}{"prefersBorder": true}}}}}}, tool, listing)
	require.NoError(t, err)
	require.Contains(t, v.CSP, "connect-src 'none';")
	require.Empty(t, v.RejectedOrigins)
	require.True(t, *v.Meta.PrefersBorder)
}

func TestBridgeLetsViewsCallOnlyTheirTools(t *testing.T) {
	ctx := context.Background()
	host, w := connect(t, "2026-07-28", true)
	listed, err := host.ListTools(ctx, nil)
	require.NoError(t, err)
	var audit []string
	bridge := apphost.NewBridge(host, listed.Tools)
	bridge.Audit = func(_ context.Context, method, name string, refused error) {
		entry := method + " " + name
		if refused != nil {
			entry += " refused"
		}
		audit = append(audit, entry)
	}

	for _, name := range []string{"get_weather", "refresh_dashboard"} {
		result, err := bridge.CallTool(ctx, &schema.CallToolRequestParams{Name: name})
		require.NoError(t, err, name)
		require.NotEmpty(t, result.Content)
	}
	sent := len(w.calls)
	for _, name := range []string{"explain_weather", "delete_everything"} {
		_, err := bridge.CallTool(ctx, &schema.CallToolRequestParams{Name: name})
		require.True(t, errors.Is(err, apphost.ErrRefused), name)
	}
	_, err = bridge.CallTool(ctx, nil)
	require.True(t, errors.Is(err, apphost.ErrRefused))
	require.Len(t, w.calls, sent, "refused calls never reach the server")

	_, err = bridge.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: "ui://weather/dashboard"})
	require.NoError(t, err)
	require.Equal(t, []string{
		"tools/call get_weather", "tools/call refresh_dashboard",
		"tools/call explain_weather refused", "tools/call delete_everything refused",
		"resources/read ui://weather/dashboard",
	}, audit)
}

// recordingSource records the parameters that reach the server.
type recordingSource struct {
	apphost.Source
	call *schema.CallToolRequestParams
	read *schema.ReadResourceRequestParams
}

func (r *recordingSource) CallTool(_ context.Context, params *schema.CallToolRequestParams, _ ...client.RequestOption) (*schema.CallToolResult, error) {
	r.call = params
	return &schema.CallToolResult{}, nil
}

func (r *recordingSource) ReadResource(_ context.Context, params *schema.ReadResourceRequestParams, _ ...client.RequestOption) (*schema.ReadResourceResult, error) {
	r.read = params
	return &schema.ReadResourceResult{}, nil
}

func TestBridgeForwardsOnlyWhatAViewMayDecide(t *testing.T) {
	ctx := context.Background()
	source := &recordingSource{}
	bridge := apphost.NewBridge(source, []schema.Tool{{Name: "refresh"}})
	_, err := bridge.CallTool(ctx, &schema.CallToolRequestParams{
		Name:      "refresh",
		Arguments: map[string]interface{}{"city": "Lisbon"},
		Meta:      schema.RequestMetaObject{IoModelcontextprotocolClientInfo: &schema.Implementation{Name: "forged"}, AdditionalProperties: map[string]interface{}{"authorization": map[string]interface{}{"token": "forged"}}},
	})
	require.NoError(t, err)
	require.Equal(t, "refresh", source.call.Name)
	require.Equal(t, map[string]interface{}{"city": "Lisbon"}, source.call.Arguments)
	require.Nil(t, source.call.Meta.IoModelcontextprotocolClientInfo)
	require.Empty(t, source.call.Meta.AdditionalProperties)

	_, err = bridge.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: "ui://x/view", Meta: schema.RequestMetaObject{IoModelcontextprotocolClientInfo: &schema.Implementation{Name: "forged"}}})
	require.NoError(t, err)
	require.Equal(t, "ui://x/view", source.read.Uri)
	require.Nil(t, source.read.Meta.IoModelcontextprotocolClientInfo)
}
