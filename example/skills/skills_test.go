package skills

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eberle1080/mcp"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/stretchr/testify/require"
)

// The walkthrough runs against the real server over Streamable HTTP in both
// protocol versions.
func TestWalkthroughOverStreamableHTTP(t *testing.T) {
	ctx := context.Background()
	compiled, err := Compile(ctx)
	require.NoError(t, err)
	day := time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC)
	srv, err := NewServer(compiled, func() time.Time { return day })
	require.NoError(t, err)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()

	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			cli, err := mcp.NewClientWithContext(ctx, Host{}, &mcp.ClientOptions{
				Name:            "skills-host",
				Version:         "1.0",
				ProtocolVersion: version,
				Transport:       mcp.ClientTransport{Type: "streamable", ClientTransportHTTP: mcp.ClientTransportHTTP{URL: endpoint.URL + "/mcp"}},
			})
			require.NoError(t, err)
			defer cli.Close()

			var out bytes.Buffer
			require.NoError(t, Walkthrough(ctx, cli, &out))
			for _, line := range []string{
				"skill skill://pdf-processing/SKILL.md (4 files): Extract, fill, and assemble PDF documents",
				"skill skill://reports/daily/SKILL.md (dynamic): Assemble today's operational report",
				"loaded pdf-processing: 231 bytes of SKILL.md, verified",
				"pdf-processing templates: invoice.md, purchase-order.md, regional/",
				"pdf-processing templates/regional: eu-invoice.md",
				"refused: skill verification failed for skill://pdf-processing/templates/credit-note.md: the held entry does not list this file",
				"loaded daily (dynamic: true)",
				"daily .: daily, sections/",
				"Summarize the open incidents as of 2026-11-27T09:30:00Z.",
			} {
				require.Contains(t, out.String(), line+"\n")
			}

			// The server lets clients cache the skill metadata, shared, for an
			// hour. Only 2026-07-28 carries those hints.
			listed, err := cli.ListSkills(ctx, nil)
			require.NoError(t, err)
			if version == schema.LatestProtocolVersion {
				require.Equal(t, 3600000, *listed.TtlMs)
				require.Equal(t, schema.CacheableResultCacheScopePublic, listed.CacheScope)
			} else {
				require.Nil(t, listed.TtlMs)
			}
		})
	}
}
