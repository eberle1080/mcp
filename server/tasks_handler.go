package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
)

func isTaskMethod(method string) bool {
	return method == tasks.MethodGet || method == tasks.MethodUpdate || method == tasks.MethodCancel
}

// taskRequest checks what every tasks/* request needs: a server with tasks,
// a 2026-07-28 request, and the client's declaration of the extension.
func (h *Handler) taskRequest(ctx context.Context, method string) *jsonrpc.Error {
	if h.Server == nil || h.tasks == nil {
		return jsonrpc.NewMethodNotFound(fmt.Sprintf("method: %v not found", method), nil)
	}
	if request, ok := ProtocolRequestFromContext(ctx); !ok || request.Version != schema.LatestProtocolVersion {
		return jsonrpc.NewMethodNotFound(fmt.Sprintf("%s is available from MCP %s", method, schema.LatestProtocolVersion), nil)
	}
	if !requestDeclaresTasks(ctx) {
		return tasks.MissingCapabilityError()
	}
	return nil
}

func decodeTaskID(raw json.RawMessage, target interface{}, id *string) *jsonrpc.Error {
	if err := json.Unmarshal(raw, target); err != nil {
		return jsonrpc.NewInvalidParamsError(fmt.Sprintf("failed to parse: %v", err), nil)
	}
	if *id == "" {
		return jsonrpc.NewInvalidParamsError("taskId is required", nil)
	}
	return nil
}

// GetTask handles tasks/get.
func (h *Handler) GetTask(ctx context.Context, request *jsonrpc.Request) (*tasks.GetTaskResult, *jsonrpc.Error) {
	if rpcErr := h.taskRequest(ctx, request.Method); rpcErr != nil {
		return nil, rpcErr
	}
	var params tasks.GetTaskRequestParams
	if rpcErr := decodeTaskID(request.Params, &params, &params.TaskID); rpcErr != nil {
		return nil, rpcErr
	}
	task, rpcErr := h.tasks.get(ctx, params.TaskID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return &tasks.GetTaskResult{ResultType: schema.ResultTypeComplete, Task: task}, nil
}

// UpdateTask handles tasks/update.
func (h *Handler) UpdateTask(ctx context.Context, request *jsonrpc.Request) (*tasks.AckResult, *jsonrpc.Error) {
	if rpcErr := h.taskRequest(ctx, request.Method); rpcErr != nil {
		return nil, rpcErr
	}
	var params tasks.UpdateTaskRequestParams
	if rpcErr := decodeTaskID(request.Params, &params, &params.TaskID); rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := h.tasks.update(ctx, params.TaskID, params.InputResponses); rpcErr != nil {
		return nil, rpcErr
	}
	return &tasks.AckResult{ResultType: schema.ResultTypeComplete}, nil
}

// CancelTask handles tasks/cancel.
func (h *Handler) CancelTask(ctx context.Context, request *jsonrpc.Request) (*tasks.AckResult, *jsonrpc.Error) {
	if rpcErr := h.taskRequest(ctx, request.Method); rpcErr != nil {
		return nil, rpcErr
	}
	var params tasks.CancelTaskRequestParams
	if rpcErr := decodeTaskID(request.Params, &params, &params.TaskID); rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := h.tasks.cancelTask(ctx, params.TaskID); rpcErr != nil {
		return nil, rpcErr
	}
	return &tasks.AckResult{ResultType: schema.ResultTypeComplete}, nil
}

// callToolWithTasks runs tools/call, letting the tool answer with a task when
// the server runs tasks and this request declared the extension. It reports
// the task the tool started, if any; the tool's own return is then ignored.
func (h *Handler) callToolWithTasks(ctx context.Context, request *jsonrpc.Request) (*schema.CallToolResult, *tasks.Task, *jsonrpc.Error) {
	if h.Server == nil || h.tasks == nil || !requestDeclaresTasks(ctx) {
		result, err := h.CallTool(ctx, request)
		return result, nil, err
	}
	runner := &requestTaskRunner{manager: h.tasks}
	result, err := h.CallTool(tasks.WithRunner(ctx, runner), request)
	if started := runner.startedTask(); started != nil {
		return nil, started, nil
	}
	return result, nil, err
}

// requestedTaskIDs reads notifications.taskIds from subscriptions/listen
// parameters, which the core schema does not model.
func requestedTaskIDs(raw json.RawMessage) ([]string, error) {
	var params struct {
		Notifications struct {
			TaskIDs []string `json:"taskIds"`
		} `json:"notifications"`
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	return params.Notifications.TaskIDs, nil
}

// taskStream queues a listen stream's task notifications and sends them from
// its own goroutine, so the task store never waits on a client. It keeps only
// the latest state of each task: a client that reads slowly skips states
// rather than making the queue grow.
type taskStream struct {
	mu      sync.Mutex
	order   []string
	latest  map[string]tasks.Task
	signal  chan struct{}
	ready   chan struct{}
	done    chan struct{}
	stopped sync.WaitGroup
}

// newTaskStream sends nothing until release: the stream's acknowledgement
// must come first.
func newTaskStream(ctx context.Context, send func(context.Context, tasks.Task)) *taskStream {
	stream := &taskStream{latest: map[string]tasks.Task{}, signal: make(chan struct{}, 1), ready: make(chan struct{}), done: make(chan struct{})}
	stream.stopped.Add(1)
	go func() {
		defer stream.stopped.Done()
		select {
		case <-stream.ready:
		case <-stream.done:
			// Released and stopped together: still send what was queued.
			select {
			case <-stream.ready:
			default:
				return
			}
		case <-ctx.Done():
			return
		}
		for {
			stream.mu.Lock()
			order, latest := stream.order, stream.latest
			stream.order, stream.latest = nil, map[string]tasks.Task{}
			stream.mu.Unlock()
			for _, id := range order {
				send(ctx, latest[id])
			}
			select {
			case <-stream.signal:
			case <-stream.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return stream
}

func (s *taskStream) enqueue(task tasks.Task) {
	s.mu.Lock()
	if _, pending := s.latest[task.TaskID]; !pending {
		s.order = append(s.order, task.TaskID)
	}
	s.latest[task.TaskID] = task
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *taskStream) release() { close(s.ready) }

func (s *taskStream) stop() {
	close(s.done)
	s.stopped.Wait()
}

// taskNotification is notifications/tasks for one stream: the full task state,
// tagged with the stream's subscription ID.
func taskNotification(subscriptionID schema.RequestId, task tasks.Task) (*jsonrpc.Notification, error) {
	raw, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	// Decode numbers as written, so the notification carries the same values
	// tasks/get would (a large integer in a result keeps every digit).
	var params map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&params); err != nil {
		return nil, err
	}
	params["_meta"] = map[string]interface{}{"io.modelcontextprotocol/subscriptionId": subscriptionID}
	raw, err = json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &jsonrpc.Notification{Jsonrpc: jsonrpc.Version, Method: tasks.MethodNotification, Params: raw}, nil
}
