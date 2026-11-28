package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/apps"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/stretchr/testify/require"
)

// upstream records the initialize request the bridge sends upstream.
type upstream struct {
	initialize schema.InitializeRequestParams
}

func (u *upstream) Notify(context.Context, *jsonrpc.Notification) error { return nil }

func (u *upstream) Send(_ context.Context, request *jsonrpc.Request) (*jsonrpc.Response, error) {
	if request.Method == schema.MethodInitialize {
		if err := json.Unmarshal(request.Params, &u.initialize); err != nil {
			return nil, err
		}
	}
	result, _ := json.Marshal(schema.InitializeResult{ProtocolVersion: u.initialize.ProtocolVersion, ServerInfo: schema.Implementation{Name: "upstream", Version: "1"}})
	return &jsonrpc.Response{Jsonrpc: jsonrpc.Version, Id: request.Id, Result: result}, nil
}

func TestTheBridgePassesOnAnAppsDeclaration(t *testing.T) {
	for _, declared := range []bool{false, true} {
		remote := &upstream{}
		ci := &clientImplementer{downstream: remote}
		init := &schema.InitializeRequestParams{ProtocolVersion: "2025-11-25", ClientInfo: schema.Implementation{Name: "desktop", Version: "1"}}
		if declared {
			init.Capabilities.Extensions = map[string]map[string]interface{}{apps.ExtensionID: apps.ClientSettings()}
		}
		result := &schema.InitializeResult{}
		ci.Initialize(context.Background(), init, result)
		require.Equal(t, "upstream", result.ServerInfo.Name)
		require.Equal(t, declared, apps.ClientSupports(&remote.initialize.Capabilities), "declared=%v", declared)
	}
}
