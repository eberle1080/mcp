package apps

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/eberle1080/mcp"
	"github.com/eberle1080/mcp-protocol/extension/apps"
	"github.com/stretchr/testify/require"
)

// The walkthrough runs against the real server over Streamable HTTP in both
// protocol versions; a client that renders no views never sees the view-only
// tool.
func TestWalkthroughOverStreamableHTTP(t *testing.T) {
	ctx := context.Background()
	observed := time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC)
	srv, err := NewServer(func() time.Time { return observed })
	require.NoError(t, err)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()

	options := func(version string, rendersViews bool) *mcp.ClientOptions {
		o := &mcp.ClientOptions{
			Name:            "weather-host",
			Version:         "1.0",
			ProtocolVersion: version,
			Transport:       mcp.ClientTransport{Type: "streamable", ClientTransportHTTP: mcp.ClientTransportHTTP{URL: endpoint.URL + "/mcp"}},
		}
		if rendersViews {
			o.Extensions = map[string]map[string]interface{}{apps.ExtensionID: apps.ClientSettings()}
		}
		return o
	}
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			cli, err := mcp.NewClientWithContext(ctx, Host{}, options(version, true))
			require.NoError(t, err)
			defer cli.Close()
			var out bytes.Buffer
			require.NoError(t, Walkthrough(ctx, cli, &out))
			for _, line := range []string{
				"server tools: explain_weather, get_weather, refresh_weather",
				"model tools: explain_weather, get_weather",
				"model called get_weather: Lisbon: 72°F and sunny (observed 2026-11-27T09:30:00Z)",
				"serve it with Content-Security-Policy: default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; media-src 'self' data:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'self'",
				`iframe allow: "", border: true`,
				"audit: view forwarded tools/call refresh_weather",
				"view refreshed: map[city:Lisbon conditions:sunny observedAt:2026-11-27T09:30:00Z temperature:72]",
				"audit: view forwarded resources/read ui://weather/dashboard",
				"audit: view refused tools/call explain_weather",
			} {
				require.Contains(t, out.String(), line+"\n")
			}

			plain, err := mcp.NewClientWithContext(ctx, Host{}, options(version, false))
			require.NoError(t, err)
			defer plain.Close()
			listed, err := plain.ListTools(ctx, nil)
			require.NoError(t, err)
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			sort.Strings(names)
			require.Equal(t, []string{"explain_weather", "get_weather"}, names)
		})
	}
}
