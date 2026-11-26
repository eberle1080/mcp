package client

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/jsonrpc/transport/client/http/streamable"
	"github.com/eberle1080/mcp-protocol/schema"
	"testing"
)

func TestLegacyHTTPVersionOnlyChangesEnvelopeMetadata(t *testing.T) {
	c := New("test", "1", &streamable.Client{}, WithProtocolVersion(schema.LegacyProtocolVersion))
	const opaque = `{ "large":9007199254740993, "_meta":{"io.modelcontextprotocol/protocolVersion":"user value"} }`
	for _, tc := range []struct{ before, after string }{
		{`{"arguments":` + opaque + `}`, `{"arguments":` + opaque + `}`},
		{`{"_meta":{"progressToken":17,"io.modelcontextprotocol/protocolVersion":""},"arguments":` + opaque + `}`, `{"_meta":{"progressToken":17,"io.modelcontextprotocol/protocolVersion":"` + schema.LegacyProtocolVersion + `"},"arguments":` + opaque + `}`},
	} {
		req := &jsonrpc.Request{Method: schema.MethodToolsCall, Params: json.RawMessage(tc.before)}
		c.withProtocolMeta(req)
		require.Equal(t, tc.after, string(req.Params))
	}
}
