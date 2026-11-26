package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/schema"
	protocolserver "github.com/eberle1080/mcp-protocol/server"
	"github.com/stretchr/testify/require"
)

// latestTestServer serves a tool that tries to elicit and a resource whose
// handler reports the pre-2026 resource-not-found code.
func latestTestServer(t *testing.T) string {
	t.Helper()
	server, err := New(WithNewHandler(protocolserver.WithDefaultHandler(context.Background(), func(handler *protocolserver.DefaultHandler) error {
		handler.Registry.RegisterToolWithSchema("ask", "Ask the user", schema.ToolInputSchema{Type: "object"}, nil, func(ctx context.Context, _ *schema.CallToolRequest) (*schema.CallToolResult, *jsonrpc.Error) {
			// The elicitation must fail at once rather than wait on a reply that a
			// 2026-07-28 client will never send.
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			_, rpcErr := handler.Client.Elicit(ctx, &jsonrpc.TypedRequest[*schema.ElicitRequest]{Request: &schema.ElicitRequest{Params: schema.ElicitRequestParams{Message: "Pick one"}}})
			text := "elicited"
			if rpcErr != nil {
				text = "elicitation refused: " + rpcErr.Message
			}
			return &schema.CallToolResult{Content: []schema.CallToolResultContentElem{schema.TextContent{Type: "text", Text: text}}}, nil
		})
		return handler.RegisterResource(schema.Resource{Uri: "test://missing", Name: "missing"}, func(_ context.Context, request *schema.ReadResourceRequest) (*schema.ReadResourceResult, *jsonrpc.Error) {
			return nil, schema.NewResourceNotFound(request.Params.Uri)
		})
	})), WithImplementation(schema.Implementation{Name: "latest", Version: "1"}))
	require.NoError(t, err)
	server.UseStreamableHTTP(true)
	endpoint := httptest.NewServer(server.HTTP(context.Background(), "127.0.0.1:0").Handler)
	t.Cleanup(endpoint.Close)
	return endpoint.URL + "/mcp"
}

type latestMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *jsonrpc.Error  `json:"error"`
}

// postLatest sends one 2026-07-28 request, accepting SSE like real clients do,
// and returns every JSON-RPC message the server wrote back.
func postLatest(t *testing.T, url, method, name, params string) []latestMessage {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(schema.HeaderProtocolVersion, schema.LatestProtocolVersion)
	request.Header.Set(schema.HeaderMethod, method)
	if name != "" {
		request.Header.Set(schema.HeaderName, name)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var messages []latestMessage
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(scanner.Bytes()), []byte("data:")))
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var message latestMessage
		require.NoError(t, json.Unmarshal(line, &message))
		messages = append(messages, message)
	}
	require.NoError(t, scanner.Err())
	return messages
}

const latestMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{}},"sampling":{},"roots":{}},"io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"}}`

func TestLatestRefusesServerToClientRequests(t *testing.T) {
	url := latestTestServer(t)
	messages := postLatest(t, url, schema.MethodToolsCall, "ask", `{"name":"ask","arguments":{},`+latestMeta+`}`)
	for _, message := range messages {
		require.Empty(t, message.Method, "2026-07-28 forbids server-to-client requests, got %s", message.Method)
	}
	require.Len(t, messages, 1)
	require.Nil(t, messages[0].Error)
	require.Contains(t, string(messages[0].Result), "elicitation refused")
}

func TestLatestRemovesPing(t *testing.T) {
	url := latestTestServer(t)
	messages := postLatest(t, url, schema.MethodPing, "", `{`+latestMeta+`}`)
	require.Len(t, messages, 1)
	require.NotNil(t, messages[0].Error)
	require.Equal(t, jsonrpc.MethodNotFound, messages[0].Error.Code)
}

func TestLatestTranslatesResourceNotFound(t *testing.T) {
	url := latestTestServer(t)
	messages := postLatest(t, url, schema.MethodResourcesRead, "test://missing", `{"uri":"test://missing",`+latestMeta+`}`)
	require.Len(t, messages, 1)
	require.NotNil(t, messages[0].Error)
	require.Equal(t, jsonrpc.InvalidParams, messages[0].Error.Code, "2026-07-28 must not emit -32002")
}

// The HTTP middleware is not the only way in (stdio, in-process adapters), so
// the runtime enforces the required client capabilities too.
func TestLatestRuntimeRequiresClientCapabilities(t *testing.T) {
	handler := &Handler{Server: &Server{}}
	request := &jsonrpc.Request{Id: 3, Jsonrpc: jsonrpc.Version, Method: schema.MethodToolsList,
		Params: json.RawMessage(`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`)}
	_, _, rpcErr := handler.prepareProtocolRequest(context.Background(), request)
	require.NotNil(t, rpcErr)
	require.Equal(t, jsonrpc.InvalidParams, rpcErr.Code)
}

func TestLatestSkillsKeepHandlerCacheHints(t *testing.T) {
	handler := &Handler{Server: &Server{}}
	ttl := 60000

	chosen := &schema.ListSkillsResult{TtlMs: &ttl, CacheScope: schema.CacheableResultCacheScopePublic}
	handler.finalizeResult(schema.LatestProtocolVersion, chosen)
	require.Equal(t, 60000, *chosen.TtlMs)
	require.Equal(t, schema.CacheableResultCacheScopePublic, chosen.CacheScope)

	unset := &schema.GetSkillResult{}
	handler.finalizeResult(schema.LatestProtocolVersion, unset)
	require.Equal(t, 0, *unset.TtlMs)
	require.Equal(t, schema.CacheableResultCacheScopePrivate, unset.CacheScope)

	legacy := &schema.GetSkillResult{TtlMs: &ttl, CacheScope: schema.CacheableResultCacheScopePublic}
	handler.finalizeResult(schema.LegacyProtocolVersion, legacy)
	require.Nil(t, legacy.TtlMs)
	require.Empty(t, legacy.CacheScope)
}
