package tasks

import (
	"bytes"
	"context"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/eberle1080/mcp"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/stretchr/testify/require"
)

func TestWalkthroughOverStreamableHTTP(t *testing.T) {
	ctx := context.Background()
	srv, err := NewServer(20 * time.Millisecond)
	require.NoError(t, err)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()
	options := func(declare bool) *mcp.ClientOptions {
		o := &mcp.ClientOptions{
			Name: "reports-host", Version: "1.0", ProtocolVersion: schema.LatestProtocolVersion,
			Transport: mcp.ClientTransport{Type: "streamable", ClientTransportHTTP: mcp.ClientTransportHTTP{URL: endpoint.URL + "/mcp"}},
		}
		if declare {
			o.Extensions = map[string]map[string]interface{}{tasks.ExtensionID: {}}
		}
		return o
	}

	cli, err := mcp.NewClientWithContext(ctx, User{}, options(true))
	require.NoError(t, err)
	defer cli.Close()
	var out bytes.Buffer
	require.NoError(t, Walkthrough(ctx, cli, &out))
	for _, line := range []string{
		"CallTool: Report for 2026-Q3, with audited and unaudited figures.",
		"task created: working (queued)",
		"task asks (elicitation/create): Some 2026-Q2 figures are not audited yet. Include them? -> no",
		"task completed: Report for 2026-Q2, with audited figures only.",
	} {
		require.Contains(t, out.String(), line+"\n")
	}
	require.Regexp(t, regexp.MustCompile(`(?m)^cancelled: task [A-Za-z0-9_-]{22} cancelled`), out.String())

	plain, err := mcp.NewClientWithContext(ctx, User{}, options(false))
	require.NoError(t, err)
	defer plain.Close()
	result, err := plain.CallTool(ctx, &schema.CallToolRequestParams{Name: "build_report", Arguments: map[string]interface{}{"quarter": "2026-Q3"}})
	require.NoError(t, err)
	require.Equal(t, "Report for 2026-Q3, with audited figures only.", text(result))
}
