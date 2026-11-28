package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp"
	"github.com/eberle1080/mcp-protocol/authorization"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/oauth2/meta"
	"github.com/eberle1080/mcp-protocol/schema"
	protocol "github.com/eberle1080/mcp-protocol/server"
	"github.com/eberle1080/mcp/client"
	"github.com/eberle1080/mcp/server"
	"github.com/eberle1080/mcp/server/auth"
	"github.com/stretchr/testify/require"
)

// taskTools registers tools that answer with tasks when they can.
func taskTools(release <-chan struct{}) protocol.NewHandler {
	return protocol.WithDefaultHandler(context.Background(), func(h *protocol.DefaultHandler) error {
		object := schema.ToolInputSchema{Type: "object"}
		text := func(s string) *schema.CallToolResult {
			return &schema.CallToolResult{Content: []schema.CallToolResultContentElem{schema.TextContent{Type: "text", Text: s}}}
		}
		startOr := func(work tasks.Work, options ...tasks.Option) protocol.ToolHandlerFunc {
			return func(ctx context.Context, _ *schema.CallToolRequest) (*schema.CallToolResult, *jsonrpc.Error) {
				if _, err := tasks.Start(ctx, work, options...); err == nil {
					return nil, nil
				} else if !errors.Is(err, tasks.ErrUnavailable) {
					return nil, jsonrpc.NewInternalError(err.Error(), nil)
				}
				return text("done without a task"), nil
			}
		}
		h.RegisterToolWithSchema("report", "Builds a report", object, nil, startOr(func(ctx context.Context, task tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
			task.Progress("gathering data")
			select {
			case <-release:
			case <-ctx.Done():
				return nil, jsonrpc.NewInternalError("stopped", nil)
			}
			return text("report ready"), nil
		}, tasks.WithPollInterval(10*time.Millisecond), tasks.WithStatusMessage("queued")))
		h.RegisterToolWithSchema("greet", "Asks for a name", object, nil, startOr(func(ctx context.Context, task tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
			answer, err := task.Elicit(ctx, &schema.ElicitRequestParams{Message: "Please enter your name.", RequestedSchema: schema.ElicitRequestParamsRequestedSchema{Type: "object", Properties: map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}})
			if err != nil {
				return nil, jsonrpc.NewInternalError(err.Error(), nil)
			}
			return text(fmt.Sprintf("Hello, %v!", answer.Content["name"])), nil
		}, tasks.WithPollInterval(10*time.Millisecond)))
		h.RegisterToolWithSchema("explode", "Fails", object, nil, startOr(func(context.Context, tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
			return nil, jsonrpc.NewInternalError("API rate limit exceeded", nil)
		}, tasks.WithPollInterval(10*time.Millisecond)))
		h.RegisterToolWithSchema("panics", "Panics", object, nil, startOr(func(context.Context, tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
			panic("boom")
		}, tasks.WithPollInterval(10*time.Millisecond)))
		h.RegisterToolWithSchema("tool_error", "Completes with isError", object, nil, startOr(func(context.Context, tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
			isError := true
			result := text("invalid input")
			result.IsError = &isError
			return result, nil
		}, tasks.WithPollInterval(10*time.Millisecond)))
		h.RegisterToolWithSchema("forever", "Runs until cancelled", object, nil, startOr(func(ctx context.Context, _ tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
			<-ctx.Done()
			return nil, jsonrpc.NewInternalError("stopped", nil)
		}, tasks.WithPollInterval(10*time.Millisecond)))
		h.RegisterToolWithSchema("needs_task", "Only runs as a task", object, nil, func(ctx context.Context, _ *schema.CallToolRequest) (*schema.CallToolResult, *jsonrpc.Error) {
			if !tasks.Available(ctx) {
				return nil, tasks.MissingCapabilityError()
			}
			if _, err := tasks.Start(ctx, func(context.Context, tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
				return text("ran as a task"), nil
			}); err != nil {
				return nil, jsonrpc.NewInternalError(err.Error(), nil)
			}
			return nil, nil
		})
		return nil
	})
}

// byClientName binds tasks to the client's self-reported name, so tests can
// play two callers.
func byClientName(ctx context.Context) string {
	if request, ok := server.ProtocolRequestFromContext(ctx); ok && request.ClientInfo != nil {
		return request.ClientInfo.Name
	}
	return ""
}

func startTaskServer(t *testing.T, release <-chan struct{}, options ...server.TaskOption) string {
	t.Helper()
	options = append([]server.TaskOption{server.WithTaskOwner(byClientName)}, options...)
	srv, err := server.New(server.WithNewHandler(taskTools(release)), server.WithTasks(options...))
	require.NoError(t, err)
	srv.UseStreamableHTTP(true)
	endpoint := httptest.NewServer(srv.HTTP(context.Background(), "127.0.0.1:0").Handler)
	t.Cleanup(endpoint.Close)
	return endpoint.URL + "/mcp"
}

// namer answers elicitations with a name.
type namer struct {
	mu    sync.Mutex
	asked int
}

func (*namer) Notify(context.Context, *jsonrpc.Notification) error   { return nil }
func (*namer) NextRequestID() jsonrpc.RequestId                      { return 0 }
func (*namer) LastRequestID() jsonrpc.RequestId                      { return 0 }
func (*namer) Init(context.Context, *schema.ClientCapabilities)      {}
func (*namer) OnNotification(context.Context, *jsonrpc.Notification) {}
func (*namer) Implements(method string) bool                         { return method == schema.MethodElicitationCreate }
func (*namer) ListRoots(context.Context, *jsonrpc.TypedRequest[*schema.ListRootsRequest]) (*schema.ListRootsResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("roots/list", nil)
}
func (*namer) CreateMessage(context.Context, *jsonrpc.TypedRequest[*schema.CreateMessageRequest]) (*schema.CreateMessageResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("sampling/createMessage", nil)
}
func (n *namer) Elicit(_ context.Context, request *jsonrpc.TypedRequest[*schema.ElicitRequest]) (*schema.ElicitResult, *jsonrpc.Error) {
	n.mu.Lock()
	n.asked++
	n.mu.Unlock()
	return &schema.ElicitResult{Action: schema.ElicitResultActionAccept, Content: map[string]interface{}{"name": "Luca"}}, nil
}

func connectTasks(t *testing.T, url, name, version string, declare bool, handler *namer) *client.Client {
	t.Helper()
	options := &mcp.ClientOptions{
		Name: name, Version: "1.0", ProtocolVersion: version,
		Transport: mcp.ClientTransport{Type: "streamable", ClientTransportHTTP: mcp.ClientTransportHTTP{URL: url}},
	}
	if declare {
		options.Extensions = map[string]map[string]interface{}{tasks.ExtensionID: {}}
	}
	if handler == nil {
		handler = &namer{}
	}
	cli, err := mcp.NewClientWithContext(context.Background(), handler, options)
	require.NoError(t, err)
	t.Cleanup(cli.Close)
	return cli
}

func textOf(t *testing.T, result *schema.CallToolResult) string {
	t.Helper()
	require.NotNil(t, result)
	require.NotEmpty(t, result.Content)
	raw, err := json.Marshal(result.Content[0])
	require.NoError(t, err)
	var item struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal(raw, &item))
	return item.Text
}

func TestTasksCallToolWaitsTransparently(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	close(release)
	url := startTaskServer(t, release)
	handler := &namer{}
	cli := connectTasks(t, url, "alice", schema.LatestProtocolVersion, true, handler)

	discovered, err := cli.Discover(ctx)
	require.NoError(t, err)
	require.Contains(t, discovered.Capabilities.Extensions, tasks.ExtensionID)

	result, err := cli.CallTool(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.Equal(t, "report ready", textOf(t, result))

	result, err = cli.CallTool(ctx, &schema.CallToolRequestParams{Name: "greet"})
	require.NoError(t, err)
	require.Equal(t, "Hello, Luca!", textOf(t, result))
	require.Equal(t, 1, handler.asked, "each input request is answered once")

	result, err = cli.CallTool(ctx, &schema.CallToolRequestParams{Name: "tool_error"})
	require.NoError(t, err, "a tool error completes the task")
	require.True(t, *result.IsError)

	for _, name := range []string{"explode", "panics"} {
		_, err = cli.CallTool(ctx, &schema.CallToolRequestParams{Name: name})
		var taskErr *client.TaskError
		require.True(t, errors.As(err, &taskErr), name)
		require.Equal(t, tasks.Failed, taskErr.Status)
		require.EqualValues(t, jsonrpc.InternalError, taskErr.Err.Code)
	}
}

func TestTasksLifecycle(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	url := startTaskServer(t, release)
	cli := connectTasks(t, url, "alice", schema.LatestProtocolVersion, true, nil)

	result, task, err := cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.Nil(t, result)
	require.NotNil(t, task)
	require.Len(t, task.TaskID, 22, "128 random bits")
	require.Equal(t, tasks.Working, task.Status)
	require.Equal(t, "queued", task.StatusMessage)
	require.EqualValues(t, 3600000, *task.TTLMs)
	require.EqualValues(t, 10, *task.PollIntervalMs)

	// The task exists as soon as the client has its ID.
	current, err := cli.GetTask(ctx, task.TaskID)
	require.NoError(t, err)
	require.False(t, current.Status.Terminal())

	require.Eventually(t, func() bool {
		current, err = cli.GetTask(ctx, task.TaskID)
		return err == nil && current.StatusMessage == "gathering data"
	}, 2*time.Second, 5*time.Millisecond)

	// Another caller cannot see, update or cancel it.
	bob := connectTasks(t, url, "bob", schema.LatestProtocolVersion, true, nil)
	for _, call := range []func() error{
		func() error { _, err := bob.GetTask(ctx, task.TaskID); return err },
		func() error {
			return bob.UpdateTask(ctx, task.TaskID, map[string]json.RawMessage{"x": json.RawMessage(`{}`)})
		},
		func() error { return bob.CancelTask(ctx, task.TaskID) },
	} {
		var rpcErr *jsonrpc.Error
		require.True(t, errors.As(call(), &rpcErr))
		require.EqualValues(t, jsonrpc.InvalidParams, rpcErr.Code)
	}
	_, err = cli.GetTask(ctx, "no-such-task")
	require.Error(t, err)

	close(release)
	final, err := cli.AwaitTask(ctx, *task)
	require.NoError(t, err)
	require.Equal(t, "report ready", textOf(t, final))
	current, err = cli.GetTask(ctx, task.TaskID)
	require.NoError(t, err)
	require.Equal(t, tasks.Completed, current.Status)
	require.JSONEq(t, `{"content":[{"type":"text","text":"report ready"}],"resultType":"complete"}`, string(current.Result))
}

func TestTasksCancellation(t *testing.T) {
	ctx := context.Background()
	url := startTaskServer(t, make(chan struct{}))
	cli := connectTasks(t, url, "alice", schema.LatestProtocolVersion, true, nil)

	_, task, err := cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "forever"})
	require.NoError(t, err)
	require.NoError(t, cli.CancelTask(ctx, task.TaskID))
	_, err = cli.AwaitTask(ctx, *task)
	var taskErr *client.TaskError
	require.True(t, errors.As(err, &taskErr))
	require.Equal(t, tasks.Cancelled, taskErr.Status)

	// A caller that stops waiting cancels the task.
	_, task, err = cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "forever"})
	require.NoError(t, err)
	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = cli.AwaitTask(waitCtx, *task)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Eventually(t, func() bool {
		current, err := cli.GetTask(ctx, task.TaskID)
		return err == nil && current.Status == tasks.Cancelled
	}, 2*time.Second, 5*time.Millisecond)
}

func TestTasksNeedTheClientsDeclaration(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	close(release)
	url := startTaskServer(t, release)

	// A 2026-07-28 client that did not declare the extension gets results.
	plain := connectTasks(t, url, "alice", schema.LatestProtocolVersion, false, nil)
	result, err := plain.CallTool(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.Equal(t, "done without a task", textOf(t, result))
	_, err = plain.CallTool(ctx, &schema.CallToolRequestParams{Name: "needs_task"})
	var rpcErr *jsonrpc.Error
	require.True(t, errors.As(err, &rpcErr), "a missing capability is a protocol error, not a tool error")
	require.EqualValues(t, tasks.ErrorCodeMissingCapability, rpcErr.Code)
	require.JSONEq(t, `{"requiredCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}`, string(rpcErr.Data))
	_, err = plain.GetTask(ctx, "anything")
	require.True(t, errors.As(err, &rpcErr))
	require.EqualValues(t, tasks.ErrorCodeMissingCapability, rpcErr.Code)

	// Before 2026-07-28 there are no tasks, declared or not.
	legacy := connectTasks(t, url, "alice", "2025-11-25", true, nil)
	result, err = legacy.CallTool(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.Equal(t, "done without a task", textOf(t, result))
	_, err = legacy.GetTask(ctx, "anything")
	require.True(t, errors.As(err, &rpcErr))
	require.EqualValues(t, jsonrpc.MethodNotFound, rpcErr.Code)

	declared := connectTasks(t, url, "alice", schema.LatestProtocolVersion, true, nil)
	result, err = declared.CallTool(ctx, &schema.CallToolRequestParams{Name: "needs_task"})
	require.NoError(t, err)
	require.Equal(t, "ran as a task", textOf(t, result))
}

func TestTasksExpireAndAreCapped(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	close(release)
	url := startTaskServer(t, release, server.WithTaskTTL(100*time.Millisecond), server.WithMaxTasks(1))
	cli := connectTasks(t, url, "alice", schema.LatestProtocolVersion, true, nil)

	_, task, err := cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.EqualValues(t, 100, *task.TTLMs)
	// At the cap the tool runs without a task.
	result, err := cli.CallTool(ctx, &schema.CallToolRequestParams{Name: "tool_error"})
	require.NoError(t, err)
	require.Equal(t, "done without a task", textOf(t, result))

	require.Eventually(t, func() bool {
		_, err := cli.GetTask(ctx, task.TaskID)
		return err != nil
	}, 2*time.Second, 10*time.Millisecond, "an expired task is gone")
	_, task, err = cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.NotNil(t, task, "expired tasks free their place")
}

func TestTasksAreOffUnlessEnabled(t *testing.T) {
	ctx := context.Background()
	srv, err := server.New(server.WithNewHandler(taskTools(nil)))
	require.NoError(t, err)
	srv.UseStreamableHTTP(true)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()
	cli := connectTasks(t, endpoint.URL+"/mcp", "alice", schema.LatestProtocolVersion, true, nil)
	discovered, err := cli.Discover(ctx)
	require.NoError(t, err)
	require.NotContains(t, discovered.Capabilities.Extensions, tasks.ExtensionID)
	result, err := cli.CallTool(ctx, &schema.CallToolRequestParams{Name: "tool_error"})
	require.NoError(t, err)
	require.Equal(t, "done without a task", textOf(t, result))
	_, err = cli.GetTask(ctx, "anything")
	var rpcErr *jsonrpc.Error
	require.True(t, errors.As(err, &rpcErr))
	require.EqualValues(t, jsonrpc.MethodNotFound, rpcErr.Code)
}

const julyTasksMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}},"io.modelcontextprotocol/clientInfo":{"name":"alice","version":"1"}}`

// post sends one 2026-07-28 request over HTTP and returns the status and every
// JSON-RPC message in the response (plain JSON or SSE).
func post(t *testing.T, ctx context.Context, url, method, name, params string, onMessage func(map[string]json.RawMessage) bool) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":` + params + `}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(schema.HeaderProtocolVersion, schema.LatestProtocolVersion)
	request.Header.Set(schema.HeaderMethod, method)
	if name != "" {
		request.Header.Set(schema.HeaderName, name)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(scanner.Bytes()), []byte("data:")))
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var message map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(line, &message))
		if onMessage != nil && !onMessage(message) {
			break
		}
	}
	return response.StatusCode
}

func TestTasksRouteByMcpNameAndNotifyListeners(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	url := startTaskServer(t, release)

	var taskID string
	post(t, ctx, url, schema.MethodToolsCall, "report", `{"name":"report",`+julyTasksMeta+`}`, func(m map[string]json.RawMessage) bool {
		var created tasks.CreateTaskResult
		if json.Unmarshal(m["result"], &created) == nil && created.ResultType == tasks.ResultTypeTask {
			taskID = created.TaskID
		}
		return true
	})
	require.NotEmpty(t, taskID)

	// tasks/* carry the task ID in Mcp-Name; a mismatch is refused.
	status := post(t, ctx, url, tasks.MethodGet, "someone-else", `{"taskId":"`+taskID+`",`+julyTasksMeta+`}`, nil)
	require.Equal(t, http.StatusBadRequest, status)
	status = post(t, ctx, url, tasks.MethodGet, "", `{"taskId":"`+taskID+`",`+julyTasksMeta+`}`, nil)
	require.Equal(t, http.StatusBadRequest, status)

	// A listen stream for the task is acknowledged with its ID, gets its current
	// state, then its completion.
	listenCtx, stopListening := context.WithTimeout(ctx, 5*time.Second)
	defer stopListening()
	var events []string
	var final tasks.Task
	done := make(chan struct{})
	go func() {
		defer close(done)
		post(t, listenCtx, url, schema.MethodSubscriptionsListen, "", `{"notifications":{"taskIds":["`+taskID+`","not-mine"]},`+julyTasksMeta+`}`, func(m map[string]json.RawMessage) bool {
			var method string
			_ = json.Unmarshal(m["method"], &method)
			switch method {
			case schema.MethodNotificationSubscriptionsAcknowledged:
				var ack struct {
					Notifications struct {
						TaskIDs []string `json:"taskIds"`
					} `json:"notifications"`
				}
				require.NoError(t, json.Unmarshal(m["params"], &ack))
				require.Equal(t, []string{taskID}, ack.Notifications.TaskIDs)
				events = append(events, "ack")
				close(release)
			case tasks.MethodNotification:
				var task struct {
					tasks.Task
					Meta map[string]interface{} `json:"_meta"`
				}
				require.NoError(t, json.Unmarshal(m["params"], &task))
				require.EqualValues(t, 7, task.Meta["io.modelcontextprotocol/subscriptionId"])
				events = append(events, string(task.Status))
				if task.Status.Terminal() {
					final = task.Task
					return false
				}
			}
			return true
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification")
	}
	require.Equal(t, "ack", events[0], "the acknowledgement comes first")
	require.Equal(t, "completed", events[len(events)-1])
	require.JSONEq(t, `{"content":[{"type":"text","text":"report ready"}],"resultType":"complete"}`, string(final.Result))

	// Asking for task notifications needs the declaration.
	var listenErr map[string]json.RawMessage
	post(t, ctx, url, schema.MethodSubscriptionsListen, "", `{"notifications":{"taskIds":["`+taskID+`"]},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`, func(m map[string]json.RawMessage) bool {
		listenErr = m
		return false
	})
	require.Contains(t, string(listenErr["error"]), "-32021")
}

// postAs sends one 2026-07-28 request with an Authorization header (if any)
// and returns the status and the first JSON-RPC message.
func postAs(t *testing.T, url, bearer, method, name, params string) (int, map[string]json.RawMessage) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":3,"method":"` + method + `","params":` + params + `}`
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(schema.HeaderProtocolVersion, schema.LatestProtocolVersion)
	request.Header.Set(schema.HeaderMethod, method)
	if name != "" {
		request.Header.Set(schema.HeaderName, name)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	var first map[string]json.RawMessage
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(scanner.Bytes()), []byte("data:")))
		if len(line) > 0 && line[0] == '{' && first == nil {
			require.NoError(t, json.Unmarshal(line, &first))
		}
	}
	return response.StatusCode, first
}

// With the standard auth middleware and the default owner, a task belongs to
// the token that created it.
func TestTasksBelongToTheirTokenBehindAuth(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	service, err := auth.New(&auth.Config{Policy: &authorization.Policy{Global: &authorization.Authorization{
		ProtectedResourceMetadata: &meta.ProtectedResourceMetadata{Resource: "https://api.example.com"},
	}}})
	require.NoError(t, err)
	srv, err := server.New(server.WithNewHandler(taskTools(release)), server.WithTasks(),
		server.WithAuthorizer(service.Middleware), server.WithJRPCAuthorizer(service.EnsureAuthorized))
	require.NoError(t, err)
	srv.UseStreamableHTTP(true)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()
	url := endpoint.URL + "/mcp"

	status, message := postAs(t, url, "alice-token", schema.MethodToolsCall, "report", `{"name":"report",`+julyTasksMeta+`}`)
	require.Equal(t, http.StatusOK, status)
	var created tasks.CreateTaskResult
	require.NoError(t, json.Unmarshal(message["result"], &created))
	require.Equal(t, tasks.ResultTypeTask, created.ResultType)
	get := `{"taskId":"` + created.TaskID + `",` + julyTasksMeta + `}`

	status, message = postAs(t, url, "alice-token", tasks.MethodGet, created.TaskID, get)
	require.Equal(t, http.StatusOK, status)
	require.Nil(t, message["error"], "the creator finds its task")

	status, message = postAs(t, url, "bob-token", tasks.MethodGet, created.TaskID, get)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, string(message["error"]), "Task not found")

	status, _ = postAs(t, url, "", tasks.MethodGet, created.TaskID, get)
	require.Equal(t, http.StatusUnauthorized, status, "task requests need credentials like any other")
	status, _ = postAs(t, url, "", schema.MethodSubscriptionsListen, "", `{"notifications":{"taskIds":["`+created.TaskID+`"]},`+julyTasksMeta+`}`)
	require.Equal(t, http.StatusUnauthorized, status)

	status, message = postAs(t, url, "alice-token", tasks.MethodCancel, created.TaskID, get)
	require.Equal(t, http.StatusOK, status)
	require.Nil(t, message["error"])
	close(release)
}

// A task waiting for input ends when the task is cancelled, even if its work
// waits on a context of its own.
func TestCancellingATaskEndsItsWaitForInput(t *testing.T) {
	ctx := context.Background()
	ended := make(chan error, 1)
	newHandler := protocol.WithDefaultHandler(ctx, func(h *protocol.DefaultHandler) error {
		h.RegisterToolWithSchema("stubborn", "Waits for input on its own context", schema.ToolInputSchema{Type: "object"}, nil, func(ctx context.Context, _ *schema.CallToolRequest) (*schema.CallToolResult, *jsonrpc.Error) {
			_, err := tasks.Start(ctx, func(_ context.Context, task tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
				_, err := task.Elicit(context.Background(), &schema.ElicitRequestParams{Message: "Anything?"})
				ended <- err
				return nil, jsonrpc.NewInternalError("no answer", nil)
			}, tasks.WithPollInterval(10*time.Millisecond))
			if err != nil {
				return nil, jsonrpc.NewInternalError(err.Error(), nil)
			}
			return nil, nil
		})
		return nil
	})
	srv, err := server.New(server.WithNewHandler(newHandler), server.WithTasks())
	require.NoError(t, err)
	srv.UseStreamableHTTP(true)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()
	cli := connectTasks(t, endpoint.URL+"/mcp", "alice", schema.LatestProtocolVersion, true, nil)

	_, task, err := cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "stubborn"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		current, err := cli.GetTask(ctx, task.TaskID)
		return err == nil && current.Status == tasks.InputRequired
	}, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, cli.CancelTask(ctx, task.TaskID))
	select {
	case err := <-ended:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("the wait for input did not end")
	}
	require.Eventually(t, func() bool {
		current, err := cli.GetTask(ctx, task.TaskID)
		return err == nil && current.Status == tasks.Cancelled
	}, 2*time.Second, 5*time.Millisecond)
}

// An expired task is stopped when its TTL elapses, not at the next request.
func TestTasksStopWhenTheyExpire(t *testing.T) {
	ctx := context.Background()
	stopped := make(chan struct{})
	newHandler := protocol.WithDefaultHandler(ctx, func(h *protocol.DefaultHandler) error {
		h.RegisterToolWithSchema("forever", "Runs until stopped", schema.ToolInputSchema{Type: "object"}, nil, func(ctx context.Context, _ *schema.CallToolRequest) (*schema.CallToolResult, *jsonrpc.Error) {
			_, err := tasks.Start(ctx, func(ctx context.Context, _ tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
				<-ctx.Done()
				close(stopped)
				return nil, nil
			}, tasks.WithTTL(50*time.Millisecond))
			if err != nil {
				return nil, jsonrpc.NewInternalError(err.Error(), nil)
			}
			return nil, nil
		})
		return nil
	})
	srv, err := server.New(server.WithNewHandler(newHandler), server.WithTasks())
	require.NoError(t, err)
	srv.UseStreamableHTTP(true)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()
	cli := connectTasks(t, endpoint.URL+"/mcp", "alice", schema.LatestProtocolVersion, true, nil)
	_, task, err := cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "forever"})
	require.NoError(t, err)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the expired task kept running")
	}
	_, err = cli.GetTask(ctx, task.TaskID)
	require.Error(t, err)
}

func TestOneCallerCannotTakeEveryTask(t *testing.T) {
	ctx := context.Background()
	url := startTaskServer(t, make(chan struct{}), server.WithMaxTasksPerCaller(1))
	alice := connectTasks(t, url, "alice", schema.LatestProtocolVersion, true, nil)
	bob := connectTasks(t, url, "bob", schema.LatestProtocolVersion, true, nil)
	_, task, err := alice.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.NotNil(t, task)
	result, task, err := alice.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.Nil(t, task, "alice is at her cap")
	require.Equal(t, "done without a task", textOf(t, result))
	_, task, err = bob.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "report"})
	require.NoError(t, err)
	require.NotNil(t, task, "bob is not")
}

// A server without tasks ignores taskIds on subscriptions/listen, as it
// always has.
func TestListenWithoutTasksIgnoresTaskIDs(t *testing.T) {
	ctx := context.Background()
	srv, err := server.New(server.WithNewHandler(taskTools(nil)))
	require.NoError(t, err)
	srv.UseStreamableHTTP(true)
	endpoint := httptest.NewServer(srv.HTTP(ctx, "127.0.0.1:0").Handler)
	defer endpoint.Close()
	var method string
	post(t, ctx, endpoint.URL+"/mcp", schema.MethodSubscriptionsListen, "", `{"notifications":{"taskIds":["x"]},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}`, func(m map[string]json.RawMessage) bool {
		_ = json.Unmarshal(m["method"], &method)
		return false
	})
	require.Equal(t, schema.MethodNotificationSubscriptionsAcknowledged, method)
}
