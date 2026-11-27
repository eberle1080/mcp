package apps

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/apps"
	"github.com/eberle1080/mcp-protocol/schema"
	serverproto "github.com/eberle1080/mcp-protocol/server"
	"github.com/eberle1080/mcp/server"
)

//go:embed view.html
var viewHTML string

// DashboardURI is the view of the weather tools.
const DashboardURI = "ui://weather/dashboard"

// CityInput names a city.
type CityInput struct {
	City string `json:"city" description:"City name, such as Lisbon"`
}

// Weather is what the tools return as structured content, for the view.
type Weather struct {
	City        string `json:"city"`
	Temperature int    `json:"temperature"`
	Conditions  string `json:"conditions"`
	ObservedAt  string `json:"observedAt"`
}

// NewHandler serves three tools and the view two of them use:
//
//   - get_weather: for the model and the view; its result is shown in the view.
//   - refresh_weather: for the view only. Hosts keep it from the model, and
//     clients that render no views never see it.
//   - explain_weather: for the model only; a view may not call it.
//
// now supplies the observation time.
func NewHandler(now func() time.Time) serverproto.NewHandler {
	return serverproto.WithDefaultHandler(context.Background(), func(h *serverproto.DefaultHandler) error {
		border := true
		// The view loads nothing from the network, so it declares no CSP
		// origins and hosts serve it under the restrictive default policy.
		if err := h.RegisterAppResource(
			schema.Resource{Uri: DashboardURI, Name: "weather_dashboard", Description: ptr("Current weather, with a refresh button")},
			apps.ResourceMeta{PrefersBorder: &border},
			serverproto.StaticHTML(viewHTML),
		); err != nil {
			return err
		}
		observe := func(_ context.Context, input *CityInput) (*schema.CallToolResult, *jsonrpc.Error) {
			if input == nil || input.City == "" {
				return nil, jsonrpc.NewInvalidParamsError("city is required", nil)
			}
			weather := Weather{City: input.City, Temperature: 72, Conditions: "sunny", ObservedAt: now().UTC().Format(time.RFC3339)}
			// The text is for the model and for hosts that show no view; the
			// structured content is what the view renders.
			return &schema.CallToolResult{
				Content:           []schema.CallToolResultContentElem{schema.TextContent{Type: "text", Text: fmt.Sprintf("%s: %d°F and %s (observed %s)", weather.City, weather.Temperature, weather.Conditions, weather.ObservedAt)}},
				StructuredContent: map[string]interface{}{"city": weather.City, "temperature": weather.Temperature, "conditions": weather.Conditions, "observedAt": weather.ObservedAt},
			}, nil
		}
		explain := func(_ context.Context, input *CityInput) (*schema.CallToolResult, *jsonrpc.Error) {
			return &schema.CallToolResult{Content: []schema.CallToolResultContentElem{schema.TextContent{Type: "text", Text: "Sunny means no cloud cover is expected in " + input.City + "."}}}, nil
		}
		for name, tool := range map[string]struct {
			description string
			handler     func(context.Context, *CityInput) (*schema.CallToolResult, *jsonrpc.Error)
			meta        apps.ToolMeta
		}{
			"get_weather":     {"Current weather for a city, with a dashboard", observe, apps.ToolMeta{ResourceURI: DashboardURI}},
			"refresh_weather": {"Refresh the weather dashboard", observe, apps.ToolMeta{ResourceURI: DashboardURI, Visibility: []apps.Visibility{apps.VisibilityApp}}},
			"explain_weather": {"Explain a city's weather in words", explain, apps.ToolMeta{Visibility: []apps.Visibility{apps.VisibilityModel}}},
		} {
			if err := serverproto.RegisterTool[*CityInput, *Weather](h.Registry, name, tool.description, tool.handler); err != nil {
				return err
			}
			if err := h.LinkToolToApp(name, tool.meta); err != nil {
				return err
			}
		}
		return nil
	})
}

// NewServer returns a Streamable HTTP MCP server for the weather app.
func NewServer(now func() time.Time) (*server.Server, error) {
	srv, err := server.New(
		server.WithNewHandler(NewHandler(now)),
		server.WithImplementation(schema.Implementation{Name: "weather-app", Version: "1.0"}),
	)
	if err != nil {
		return nil, err
	}
	srv.UseStreamableHTTP(true)
	return srv, nil
}

func ptr[T any](v T) *T { return &v }
