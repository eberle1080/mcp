package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eberle1080/mcp-protocol/schema"
)

func TestProtocolVersionMiddlewareRequiresRoutingHeadersOnlyForModernRequests(t *testing.T) {
	for _, version := range []string{"2025-06-18", "2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			called := false
			handler := protocolVersionMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))
			body := `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"` + version + `","io.modelcontextprotocol/clientCapabilities":{}}}}`
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			request.Header.Set(schema.HeaderProtocolVersion, version)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := http.StatusNoContent
			if version == "2026-07-28" {
				want = http.StatusBadRequest
			}
			if response.Code != want || called != (want == http.StatusNoContent) {
				t.Fatalf("version %s without routing headers: status=%d called=%v want=%d", version, response.Code, called, want)
			}
		})
	}
}

func TestNegotiatedProtocolVersion(t *testing.T) {
	testCases := []struct {
		name      string
		requested string
		expect    string
	}{
		{name: "empty defaults to latest", expect: schema.LatestProtocolVersion},
		{name: "latest preserved", requested: schema.LatestProtocolVersion, expect: schema.LatestProtocolVersion},
		{name: "older supported version preserved", requested: "2025-06-18", expect: "2025-06-18"},
		{name: "legacy preserved", requested: schema.LegacyProtocolVersion, expect: schema.LegacyProtocolVersion},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			actual := negotiatedProtocolVersion(testCase.requested)
			if actual != testCase.expect {
				t.Fatalf("expected %q, got %q", testCase.expect, actual)
			}
		})
	}
}

func TestProtocolVersionMiddleware_ValidatesJulyStandardHeaders(t *testing.T) {
	handler := protocolVersionMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"weather","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set(schema.HeaderProtocolVersion, schema.LatestProtocolVersion)
	request.Header.Set(schema.HeaderMethod, schema.MethodToolsCall)
	request.Header.Set(schema.HeaderName, "weather")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, response.Code, response.Body.String())
	}
}

func TestProtocolVersionMiddleware_RejectsJulyHeaderMismatch(t *testing.T) {
	handler := protocolVersionMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mismatched request reached handler")
	}))
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"weather","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set(schema.HeaderProtocolVersion, schema.LatestProtocolVersion)
	request.Header.Set(schema.HeaderMethod, schema.MethodToolsCall)
	request.Header.Set(schema.HeaderName, "calendar")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, response.Code)
	}
	var result struct {
		ID    int `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != 7 || result.Error.Code != schema.ErrorCodeHeaderMismatch {
		t.Fatalf("unexpected error response: %s", response.Body.String())
	}
}

func TestProtocolVersionMiddleware_RequiresJulyBodyVersion(t *testing.T) {
	handler := protocolVersionMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("invalid request reached handler")
	}))
	body := `{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{"_meta":{}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set(schema.HeaderProtocolVersion, schema.LatestProtocolVersion)
	request.Header.Set(schema.HeaderMethod, schema.MethodToolsList)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, response.Code)
	}
}

func TestProtocolVersionMiddleware_UsesNegotiatedVersion(t *testing.T) {
	handler := protocolVersionMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("MCP-Protocol-Version", "2025-06-18")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if actual := response.Header().Get("MCP-Protocol-Version"); actual != "2025-06-18" {
		t.Fatalf("expected negotiated protocol header %q, got %q", "2025-06-18", actual)
	}
}

func TestProtocolVersionMiddleware_RejectsUnsupportedVersion(t *testing.T) {
	handler := protocolVersionMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unsupported request reached handler")
	}))
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("MCP-Protocol-Version", "2026-01-01")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, response.Code)
	}
}

// serveProtocol runs one POST through the middleware and returns the response;
// reached reports whether the request got past it.
func serveProtocol(t *testing.T, header map[string]string, body string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	handler := protocolVersionMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	for key, value := range header {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response, reached
}

func protocolErrorOf(t *testing.T, response *httptest.ResponseRecorder) (json.RawMessage, int) {
	t.Helper()
	var result struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("not a JSON-RPC error: %s", response.Body.String())
	}
	return result.ID, result.Error.Code
}

var julyToolsList = map[string]string{
	schema.HeaderProtocolVersion: schema.LatestProtocolVersion,
	schema.HeaderMethod:          schema.MethodToolsList,
}

func TestProtocolVersionMiddleware_RequiresJulyClientCapabilities(t *testing.T) {
	response, reached := serveProtocol(t, julyToolsList,
		`{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	id, code := protocolErrorOf(t, response)
	if reached || response.Code != http.StatusBadRequest || code != -32602 || string(id) != "11" {
		t.Fatalf("missing clientCapabilities: reached=%v status=%d body=%s", reached, response.Code, response.Body.String())
	}
}

func TestProtocolVersionMiddleware_RejectsJulyBatch(t *testing.T) {
	response, reached := serveProtocol(t, julyToolsList,
		`[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}]`)
	_, code := protocolErrorOf(t, response)
	if reached || response.Code != http.StatusBadRequest || code != -32600 {
		t.Fatalf("batch: reached=%v status=%d body=%s", reached, response.Code, response.Body.String())
	}
}

func TestProtocolVersionMiddleware_DecodesBase64Name(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"météo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	header := func(name string) map[string]string {
		return map[string]string{
			schema.HeaderProtocolVersion: schema.LatestProtocolVersion,
			schema.HeaderMethod:          schema.MethodToolsCall,
			schema.HeaderName:            name,
		}
	}
	// "météo" is not ASCII, so the client sends it base64-encoded.
	if response, reached := serveProtocol(t, header("=?base64?bcOpdMOpbw==?="), body); !reached {
		t.Fatalf("encoded name rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, bad := range []string{"=?base64?not base64!?=", "=?base64?bWV0ZW8=?="} {
		response, reached := serveProtocol(t, header(bad), body)
		if _, code := protocolErrorOf(t, response); reached || code != schema.ErrorCodeHeaderMismatch {
			t.Fatalf("%q: reached=%v body=%s", bad, reached, response.Body.String())
		}
	}
}

func TestProtocolVersionMiddleware_UnsupportedVersionEchoesID(t *testing.T) {
	response, reached := serveProtocol(t, map[string]string{schema.HeaderProtocolVersion: "2026-01-01"},
		`{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}`)
	id, code := protocolErrorOf(t, response)
	if reached || code != schema.ErrorCodeUnsupportedProtocolVersion || string(id) != "9" {
		t.Fatalf("unsupported version: reached=%v body=%s", reached, response.Body.String())
	}
}

func TestProtocolVersionMiddleware_NoVersionHeaderForLegacyRequests(t *testing.T) {
	response, _ := serveProtocol(t, nil,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy","version":"1"}}}`)
	if actual := response.Header().Get(schema.HeaderProtocolVersion); actual != "" {
		t.Fatalf("legacy request without a version header was told %q", actual)
	}
}
