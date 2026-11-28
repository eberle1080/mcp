package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/stretchr/testify/require"
)

// taskServer is a scripted 2026-07-28 server: tools/call answers with a task,
// and each tasks/get returns the next state in the script.
type taskServer struct {
	mu       sync.Mutex
	states   []string
	methods  []string
	ids      []string
	updates  []map[string]json.RawMessage
	failGets int // tasks/get fails this many times first
}

func (s *taskServer) send(_ context.Context, r *jsonrpc.Request) (*jsonrpc.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, r.Method)
	s.ids = append(s.ids, fmt.Sprint(r.Id))
	if r.Method == tasks.MethodGet && s.failGets > 0 {
		s.failGets--
		return nil, errors.New("connection reset")
	}
	reply := func(v string) (*jsonrpc.Response, error) {
		return &jsonrpc.Response{Jsonrpc: jsonrpc.Version, Id: r.Id, Result: json.RawMessage(v)}, nil
	}
	switch r.Method {
	case schema.MethodServerDiscover:
		return reply(`{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}},"ttlMs":0,"cacheScope":"public"}`)
	case schema.MethodToolsCall:
		return reply(`{"resultType":"task","taskId":"t1","status":"working","createdAt":"t","lastUpdatedAt":"t","ttlMs":null,"pollIntervalMs":1}`)
	case tasks.MethodGet:
		next := s.states[0]
		if len(s.states) > 1 {
			s.states = s.states[1:]
		}
		return reply(next)
	case tasks.MethodUpdate:
		var params tasks.UpdateTaskRequestParams
		_ = json.Unmarshal(r.Params, &params)
		s.updates = append(s.updates, params.InputResponses)
		return reply(`{"resultType":"complete"}`)
	case tasks.MethodCancel:
		return reply(`{"resultType":"complete"}`)
	}
	return &jsonrpc.Response{Jsonrpc: jsonrpc.Version, Id: r.Id, Error: jsonrpc.NewMethodNotFound(r.Method, nil)}, nil
}

const (
	asksName  = `{"resultType":"complete","taskId":"t1","status":"input_required","createdAt":"t","lastUpdatedAt":"t","ttlMs":null,"pollIntervalMs":1,"inputRequests":{"name":{"method":"elicitation/create","params":{"mode":"form","message":"Your name?","requestedSchema":{"type":"object","properties":{}}}}}}`
	asksModel = `{"resultType":"complete","taskId":"t1","status":"input_required","createdAt":"t","lastUpdatedAt":"t","ttlMs":null,"pollIntervalMs":1,"inputRequests":{"m":{"method":"sampling/createMessage","params":{"messages":[],"maxTokens":5}}}}`
	completed = `{"resultType":"complete","taskId":"t1","status":"completed","createdAt":"t","lastUpdatedAt":"t","ttlMs":null,"result":{"content":[{"type":"text","text":"Hello, Luca!"}],"resultType":"complete"}}`
)

// elicitor answers elicitations and nothing else.
type elicitor struct{ asked int }

func (*elicitor) Notify(context.Context, *jsonrpc.Notification) error   { return nil }
func (*elicitor) NextRequestID() jsonrpc.RequestId                      { return 0 }
func (*elicitor) LastRequestID() jsonrpc.RequestId                      { return 0 }
func (*elicitor) Init(context.Context, *schema.ClientCapabilities)      {}
func (*elicitor) OnNotification(context.Context, *jsonrpc.Notification) {}
func (*elicitor) Implements(method string) bool                         { return method == schema.MethodElicitationCreate }
func (*elicitor) ListRoots(context.Context, *jsonrpc.TypedRequest[*schema.ListRootsRequest]) (*schema.ListRootsResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("roots/list", nil)
}
func (*elicitor) CreateMessage(context.Context, *jsonrpc.TypedRequest[*schema.CreateMessageRequest]) (*schema.CreateMessageResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("sampling/createMessage", nil)
}
func (e *elicitor) Elicit(_ context.Context, r *jsonrpc.TypedRequest[*schema.ElicitRequest]) (*schema.ElicitResult, *jsonrpc.Error) {
	e.asked++
	return &schema.ElicitResult{Action: schema.ElicitResultActionAccept, Content: map[string]interface{}{"name": "Luca"}}, nil
}

func taskClient(server *taskServer, declare bool, handler *elicitor) *Client {
	options := []Option{WithProtocolVersion(schema.LatestProtocolVersion)}
	if handler != nil {
		options = append(options, WithClientHandler(handler))
	}
	if declare {
		options = append(options, WithExtension(tasks.ExtensionID, nil))
	}
	return New("host", "1", &mockTransport{send: server.send}, options...)
}

func TestAwaitTaskAnswersEachInputRequestOnce(t *testing.T) {
	server := &taskServer{states: []string{asksName, asksName, completed}}
	handler := &elicitor{}
	c := taskClient(server, true, handler)
	result, err := c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "greet"})
	require.NoError(t, err)
	require.Equal(t, "Hello, Luca!", result.Content[0].(map[string]interface{})["text"])
	require.Equal(t, 1, handler.asked, "a request seen on two polls is answered once")
	require.Len(t, server.updates, 1)
	require.JSONEq(t, `{"action":"accept","content":{"name":"Luca"}}`, string(server.updates[0]["name"]))
}

func TestAwaitTaskGivesUpOnInputItCannotProvide(t *testing.T) {
	server := &taskServer{states: []string{asksModel}}
	c := taskClient(server, true, &elicitor{})
	_, err := c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "summarize"})
	require.ErrorContains(t, err, "sampling/createMessage")
	require.Contains(t, server.methods, tasks.MethodCancel, "the abandoned task is cancelled")
}

func TestATaskForAClientThatDidNotAskIsInvalid(t *testing.T) {
	server := &taskServer{}
	c := taskClient(server, false, nil)
	_, err := c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "greet"})
	require.Error(t, err, "a non-declaring client cannot read a task as a CallToolResult")
	_, _, err = c.CallToolTask(context.Background(), &schema.CallToolRequestParams{Name: "greet"})
	require.ErrorContains(t, err, "did not declare")
}

func TestTaskErrors(t *testing.T) {
	failed := `{"resultType":"complete","taskId":"t1","status":"failed","statusMessage":"rate limited","createdAt":"t","lastUpdatedAt":"t","ttlMs":null,"error":{"code":-32603,"message":"API rate limit exceeded"}}`
	c := taskClient(&taskServer{states: []string{failed}}, true, nil)
	_, err := c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "x"})
	var taskErr *TaskError
	require.True(t, errors.As(err, &taskErr))
	require.Equal(t, tasks.Failed, taskErr.Status)
	var rpcErr *jsonrpc.Error
	require.True(t, errors.As(err, &rpcErr))
	require.EqualValues(t, -32603, rpcErr.Code)

	cancelled := `{"resultType":"complete","taskId":"t1","status":"cancelled","createdAt":"t","lastUpdatedAt":"t","ttlMs":null}`
	c = taskClient(&taskServer{states: []string{cancelled}}, true, nil)
	_, err = c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "x"})
	require.True(t, errors.As(err, &taskErr))
	require.Equal(t, tasks.Cancelled, taskErr.Status)
}

func TestPollsDoNotReuseTheCallsRequestID(t *testing.T) {
	server := &taskServer{states: []string{completed}}
	c := taskClient(server, true, nil)
	_, err := c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "x"}, WithJsonRpcRequestId(42))
	require.NoError(t, err)
	for i, method := range server.methods {
		if method == schema.MethodToolsCall {
			require.Equal(t, "42", server.ids[i])
		} else if method == tasks.MethodGet {
			require.NotEqual(t, "42", server.ids[i])
		}
	}
}

func TestAPollThatFailsIsRetried(t *testing.T) {
	server := &taskServer{states: []string{completed}, failGets: 2}
	c := taskClient(server, true, nil)
	result, err := c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "x"})
	require.NoError(t, err)
	require.Equal(t, "Hello, Luca!", result.Content[0].(map[string]interface{})["text"])

	server = &taskServer{states: []string{completed}, failGets: 10}
	c = taskClient(server, true, nil)
	_, err = c.CallTool(context.Background(), &schema.CallToolRequestParams{Name: "x"})
	require.ErrorContains(t, err, "connection reset")
	require.Contains(t, server.methods, tasks.MethodCancel, "a task given up on is cancelled")
}
