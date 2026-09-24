package client

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/stretchr/testify/require"
)

func TestListenUsesJulyMetadata(t *testing.T) {
	transport := &mockTransport{send: func(_ context.Context, request *jsonrpc.Request) (*jsonrpc.Response, error) {
		require.Equal(t, schema.MethodSubscriptionsListen, request.Method)
		var params map[string]interface{}
		require.NoError(t, json.Unmarshal(request.Params, &params))
		meta := params["_meta"].(map[string]interface{})
		require.Equal(t, schema.LatestProtocolVersion, meta["io.modelcontextprotocol/protocolVersion"])
		result := schema.SubscriptionsListenResult{
			Meta:       schema.SubscriptionsListenResultMetaObject{IoModelcontextprotocolSubscriptionId: 9},
			ResultType: schema.ResultTypeComplete,
		}
		raw, err := json.Marshal(result)
		require.NoError(t, err)
		return &jsonrpc.Response{Jsonrpc: jsonrpc.Version, Result: raw}, nil
	}}
	client := &Client{transport: transport, initialized: true, protocolVersion: schema.LatestProtocolVersion, info: schema.Implementation{Name: "test", Version: "1"}}
	result, err := client.Listen(context.Background(), schema.SubscriptionFilter{})
	require.NoError(t, err)
	require.Equal(t, schema.RequestId(9), result.Meta.IoModelcontextprotocolSubscriptionId)
}
