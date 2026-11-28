package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
)

const maxProtocolRequestBody = 4 << 20

// protocolVersionMiddleware validates the transport-level protocol rules and
// sets the response MCP-Protocol-Version header for requests that sent one.
// Negotiation should happen at initialize time; transport-level requests should
// not be rejected solely due to a newer client-advertised version.
func protocolVersionMiddleware(configuredLimit ...int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested := strings.TrimSpace(r.Header.Get(schema.HeaderProtocolVersion))
			var body []byte
			if r.Method == http.MethodPost && r.Body != nil {
				var err error
				if body, err = readProtocolRequestBody(w, r, configuredLimit...); err != nil {
					return
				}
			}
			if requested != "" && !supportedProtocolVersion(requested) {
				writeProtocolError(w, requestID(body), schema.ErrorCodeUnsupportedProtocolVersion,
					fmt.Sprintf("unsupported MCP protocol version %q", requested), map[string]interface{}{
						"requested": requested,
						"supported": schema.SupportedProtocolVersions,
					})
				return
			}
			if body != nil {
				if err := validateProtocolHTTPHeaders(r.Header, requested, body); err != nil {
					writeProtocolError(w, err.id, err.code, err.message, nil)
					return
				}
			}
			// A client that sent no version header is a legacy client whose version
			// is settled by initialize; don't announce the latest revision to it.
			if requested != "" {
				w.Header().Set(schema.HeaderProtocolVersion, negotiatedProtocolVersion(requested))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requestID returns the JSON-RPC id of a single request body, or nil.
func requestID(body []byte) json.RawMessage {
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	return request.ID
}

type protocolHeaderError struct {
	id      json.RawMessage
	code    int
	message string
}

func (e *protocolHeaderError) Error() string { return e.message }

func readProtocolRequestBody(w http.ResponseWriter, r *http.Request, configuredLimit ...int64) ([]byte, error) {
	limit := int64(maxProtocolRequestBody)
	if len(configuredLimit) > 0 && configuredLimit[0] > 0 {
		limit = configuredLimit[0]
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		http.Error(w, "failed to read MCP request body", http.StatusBadRequest)
		return nil, err
	}
	_ = r.Body.Close()
	if int64(len(body)) > limit {
		http.Error(w, "MCP request body is too large", http.StatusRequestEntityTooLarge)
		return nil, fmt.Errorf("MCP request body exceeds %d bytes", limit)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return body, nil
}

func validateProtocolHTTPHeaders(header http.Header, headerVersion string, body []byte) *protocolHeaderError {
	trimmed := bytes.TrimSpace(body)
	// A 2026-07-28 POST carries exactly one message, so a JSON-RPC batch is an
	// invalid request rather than something to accept and silently drop.
	if headerVersion == schema.LatestProtocolVersion && len(trimmed) > 0 && trimmed[0] == '[' {
		return newProtocolHeaderError(nil, jsonrpc.InvalidRequest,
			fmt.Sprintf("JSON-RPC batches are not supported in MCP %s", schema.LatestProtocolVersion))
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if len(trimmed) == 0 || json.Unmarshal(body, &request) != nil || request.Method == "" {
		return nil // JSON-RPC decoding reports malformed messages downstream.
	}
	// The July routing headers and per-request version metadata apply to calls,
	// not notifications.
	if len(request.ID) == 0 || string(request.ID) == "null" {
		return nil
	}
	metaVersion := protocolVersionFromParams(request.Params)
	usesJuly := headerVersion == schema.LatestProtocolVersion || metaVersion == schema.LatestProtocolVersion
	if !usesJuly {
		return nil
	}
	if headerVersion == "" {
		return newProtocolHeaderError(request.ID, schema.ErrorCodeHeaderMismatch,
			fmt.Sprintf("%s header is required", schema.HeaderProtocolVersion))
	}
	if metaVersion == "" {
		return newProtocolHeaderError(request.ID, -32602,
			fmt.Sprintf("missing or invalid _meta field %q", "io.modelcontextprotocol/protocolVersion"))
	}
	if headerVersion != metaVersion {
		return newProtocolHeaderError(request.ID, schema.ErrorCodeHeaderMismatch,
			fmt.Sprintf("%s header %q does not match request protocol version %q", schema.HeaderProtocolVersion, headerVersion, metaVersion))
	}
	if !hasClientCapabilities(request.Params) {
		return newProtocolHeaderError(request.ID, jsonrpc.InvalidParams,
			fmt.Sprintf("missing or invalid _meta field %q", "io.modelcontextprotocol/clientCapabilities"))
	}
	method := strings.TrimSpace(header.Get(schema.HeaderMethod))
	if method == "" {
		return newProtocolHeaderError(request.ID, schema.ErrorCodeHeaderMismatch,
			fmt.Sprintf("missing required %s header", schema.HeaderMethod))
	}
	if method != request.Method {
		return newProtocolHeaderError(request.ID, schema.ErrorCodeHeaderMismatch,
			fmt.Sprintf("%s header %q does not match request method %q", schema.HeaderMethod, method, request.Method))
	}
	field := standardHeaderNameField(request.Method)
	if field == "" {
		return nil
	}
	name := protocolNameFromParams(request.Params, field)
	rawHeaderName := strings.TrimSpace(header.Get(schema.HeaderName))
	if rawHeaderName == "" {
		return newProtocolHeaderError(request.ID, schema.ErrorCodeHeaderMismatch,
			fmt.Sprintf("missing required %s header for method %q", schema.HeaderName, request.Method))
	}
	headerName, ok := decodeHeaderValue(rawHeaderName)
	if !ok {
		return newProtocolHeaderError(request.ID, schema.ErrorCodeHeaderMismatch,
			fmt.Sprintf("%s header %q is not a valid encoded value", schema.HeaderName, rawHeaderName))
	}
	if name == "" || headerName != name {
		return newProtocolHeaderError(request.ID, schema.ErrorCodeHeaderMismatch,
			fmt.Sprintf("%s header %q does not match request %s %q", schema.HeaderName, rawHeaderName, field, name))
	}
	return nil
}

// headerValuePrefix and headerValueSuffix mark a header value the client
// base64-encoded because it was not plain ASCII (2026-07-28 Streamable HTTP).
const (
	headerValuePrefix = "=?base64?"
	headerValueSuffix = "?="
)

// decodeHeaderValue returns a routing header's value, decoding the
// =?base64?...?= form. ok is false when an encoded value is malformed.
func decodeHeaderValue(value string) (decoded string, ok bool) {
	if !strings.HasPrefix(value, headerValuePrefix) || !strings.HasSuffix(value, headerValueSuffix) ||
		len(value) < len(headerValuePrefix)+len(headerValueSuffix) {
		return value, true
	}
	raw, err := base64.StdEncoding.DecodeString(value[len(headerValuePrefix) : len(value)-len(headerValueSuffix)])
	if err != nil || !utf8.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

// hasClientCapabilities reports whether a request's _meta carries the
// io.modelcontextprotocol/clientCapabilities object 2026-07-28 requires.
func hasClientCapabilities(raw json.RawMessage) bool {
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return false
	}
	value := bytes.TrimSpace(params.Meta["io.modelcontextprotocol/clientCapabilities"])
	return len(value) > 0 && value[0] == '{'
}

func newProtocolHeaderError(id json.RawMessage, code int, message string) *protocolHeaderError {
	return &protocolHeaderError{id: id, code: code, message: message}
}

func protocolVersionFromParams(raw json.RawMessage) string {
	var params struct {
		Meta map[string]interface{} `json:"_meta"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return ""
	}
	version, _ := params.Meta["io.modelcontextprotocol/protocolVersion"].(string)
	return version
}

func protocolNameFromParams(raw json.RawMessage, field string) string {
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil {
		return ""
	}
	var value string
	_ = json.Unmarshal(params[field], &value)
	return value
}

func standardHeaderNameField(method string) string {
	switch method {
	case schema.MethodToolsCall, schema.MethodPromptsGet:
		return "name"
	case schema.MethodResourcesRead:
		return "uri"
	case tasks.MethodGet, tasks.MethodUpdate, tasks.MethodCancel:
		// Routes a task's requests to the instance that holds it.
		return "taskId"
	default:
		return ""
	}
}

func writeProtocolError(w http.ResponseWriter, id json.RawMessage, code int, message string, data interface{}) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	errorValue := map[string]interface{}{"code": code, "message": message}
	if data != nil {
		errorValue["data"] = data
	}
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   errorValue,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(response)
}

func negotiatedProtocolVersion(requested string) string {
	requested = strings.TrimSpace(requested)
	switch requested {
	case "", schema.LatestProtocolVersion:
		return schema.LatestProtocolVersion
	case schema.LegacyProtocolVersion, "2025-06-18":
		return requested
	default:
		return schema.LatestProtocolVersion
	}
}
