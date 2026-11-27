package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
)

// Polling bounds for AwaitTask: a server's suggested interval is honoured
// within them, so a bad value can neither spin the client nor stall it.
const (
	defaultTaskPoll = time.Second
	minTaskPoll     = 50 * time.Millisecond
	maxTaskPoll     = time.Minute
	// taskPollAttempts is how many times one poll is tried before AwaitTask
	// gives up on the task.
	taskPollAttempts = 3
)

// declaresTasks reports whether this client declared the Tasks extension.
func (c *Client) declaresTasks() bool {
	return tasks.ClientSupports(&c.capabilities)
}

// CallToolTask calls a tool and returns either its result or, when the
// server answered with a task (the Tasks extension, MCP 2026-07-28), the task.
// A client that declared the extension with
// WithExtension(tasks.ExtensionID, nil) may get either.
func (c *Client) CallToolTask(ctx context.Context, params *schema.CallToolRequestParams, options ...RequestOption) (*schema.CallToolResult, *tasks.Task, error) {
	raw, err := sendRaw(ctx, c, schema.MethodToolsCall, params, options...)
	if err != nil {
		return nil, nil, err
	}
	var kind struct {
		ResultType string `json:"resultType"`
	}
	_ = json.Unmarshal(raw, &kind)
	if kind.ResultType != tasks.ResultTypeTask {
		result, err := decodeResult[schema.CallToolResult](schema.MethodToolsCall, raw)
		return result, nil, err
	}
	if !c.declaresTasks() {
		return nil, nil, jsonrpc.NewInternalError("the server answered with a task, but this client did not declare the tasks extension", nil)
	}
	var created tasks.CreateTaskResult
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, nil, jsonrpc.NewInternalError(fmt.Sprintf("invalid task result: %v", err), nil)
	}
	if created.TaskID == "" {
		return nil, nil, jsonrpc.NewInternalError("the server's task result has no taskId", nil)
	}
	return nil, &created.Task, nil
}

// GetTask returns a task's current state.
func (c *Client) GetTask(ctx context.Context, taskID string, options ...RequestOption) (*tasks.Task, error) {
	result, err := send[tasks.GetTaskRequestParams, tasks.GetTaskResult](ctx, c, tasks.MethodGet, &tasks.GetTaskRequestParams{TaskID: taskID}, options...)
	if err != nil {
		return nil, err
	}
	if result.TaskID != taskID {
		return nil, jsonrpc.NewInternalError(fmt.Sprintf("asked for task %s, got %s", taskID, result.TaskID), nil)
	}
	return &result.Task, nil
}

// UpdateTask answers some of a task's input requests, keyed as the task
// listed them.
func (c *Client) UpdateTask(ctx context.Context, taskID string, responses map[string]json.RawMessage, options ...RequestOption) error {
	_, err := send[tasks.UpdateTaskRequestParams, tasks.AckResult](ctx, c, tasks.MethodUpdate, &tasks.UpdateTaskRequestParams{TaskID: taskID, InputResponses: responses}, options...)
	return err
}

// CancelTask asks the server to stop a task. Cancellation is cooperative: the
// task may still finish another way.
func (c *Client) CancelTask(ctx context.Context, taskID string, options ...RequestOption) error {
	_, err := send[tasks.CancelTaskRequestParams, tasks.AckResult](ctx, c, tasks.MethodCancel, &tasks.CancelTaskRequestParams{TaskID: taskID}, options...)
	return err
}

// TaskError is a task that ended without a result: it failed with a JSON-RPC
// error (Err is set) or was cancelled.
type TaskError struct {
	TaskID string
	Status tasks.Status
	// Message is the task's status message.
	Message string
	// Err is the JSON-RPC error a failed task carries.
	Err *jsonrpc.Error
}

func (e *TaskError) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("task %s failed: %s", e.TaskID, e.Err.Message)
	case e.Message != "":
		return fmt.Sprintf("task %s %s: %s", e.TaskID, e.Status, e.Message)
	default:
		return fmt.Sprintf("task %s %s", e.TaskID, e.Status)
	}
}

func (e *TaskError) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return nil
}

// AwaitTask polls a task until it ends and returns its result. It honours
// the server's poll interval. It answers the task's input requests
// (elicitation, sampling, roots) with this client's handler, as it would the
// same requests sent directly, and answers each one once. If ctx ends first,
// it asks the server to cancel the task. A failed or cancelled task is a
// *TaskError.
func (c *Client) AwaitTask(ctx context.Context, task tasks.Task, options ...RequestOption) (*schema.CallToolResult, error) {
	answered := map[string]bool{}
	current := task
	// stopped ends the wait because ctx ended: the task is cancelled, since no
	// one is waiting for it any more.
	stopped := func() error {
		c.abandonTask(ctx, current.TaskID, options...)
		return ctx.Err()
	}
	// refresh polls once more. A failed poll is retried a few times at the
	// poll interval before the wait is given up, and the task with it.
	refresh := func() error {
		var err error
		for attempt := 0; attempt < taskPollAttempts; attempt++ {
			if attempt > 0 {
				timer := time.NewTimer(pollInterval(current))
				select {
				case <-ctx.Done():
					timer.Stop()
					return stopped()
				case <-timer.C:
				}
			}
			var next *tasks.Task
			if next, err = c.GetTask(ctx, current.TaskID, options...); err == nil {
				current = *next
				return nil
			}
			if ctx.Err() != nil {
				return stopped()
			}
			var rpcErr *jsonrpc.Error
			if errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.InvalidParams {
				return err // the task is gone: nothing to retry or cancel
			}
		}
		c.abandonTask(ctx, current.TaskID, options...)
		return err
	}
	for {
		switch current.Status {
		case tasks.Completed:
			if len(bytes.TrimSpace(current.Result)) == 0 {
				return nil, jsonrpc.NewInternalError(fmt.Sprintf("completed task %s has no result", current.TaskID), nil)
			}
			return decodeResult[schema.CallToolResult](schema.MethodToolsCall, current.Result)
		case tasks.Failed:
			return nil, &TaskError{TaskID: current.TaskID, Status: current.Status, Message: current.StatusMessage, Err: current.Error}
		case tasks.Cancelled:
			return nil, &TaskError{TaskID: current.TaskID, Status: current.Status, Message: current.StatusMessage}
		case tasks.InputRequired:
			responses, err := c.answerInputs(ctx, current, answered)
			if err != nil {
				c.abandonTask(ctx, current.TaskID, options...)
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, err
			}
			if len(responses) > 0 {
				if err := c.UpdateTask(ctx, current.TaskID, responses, options...); err != nil {
					if ctx.Err() != nil {
						return nil, stopped()
					}
					return nil, err
				}
				for key := range responses {
					answered[key] = true
				}
				// Look again at once: the answers may have moved the task on.
				if err := refresh(); err != nil {
					return nil, err
				}
				continue
			}
		}
		timer := time.NewTimer(pollInterval(current))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, stopped()
		case <-timer.C:
		}
		if err := refresh(); err != nil {
			return nil, err
		}
	}
}

func pollInterval(task tasks.Task) time.Duration {
	interval := defaultTaskPoll
	if task.PollIntervalMs != nil {
		interval = time.Duration(*task.PollIntervalMs) * time.Millisecond
	}
	if interval < minTaskPoll {
		return minTaskPoll
	}
	if interval > maxTaskPoll {
		return maxTaskPoll
	}
	return interval
}

// abandonTask cancels a task the caller has stopped waiting for. The caller's
// context may already be done, so the request gets its own short deadline.
func (c *Client) abandonTask(ctx context.Context, taskID string, options ...RequestOption) {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = c.CancelTask(cancelCtx, taskID, options...)
}

// answerInputs answers the task's input requests that have not been answered,
// with the client's handler.
func (c *Client) answerInputs(ctx context.Context, task tasks.Task, answered map[string]bool) (map[string]json.RawMessage, error) {
	responses := map[string]json.RawMessage{}
	for key, request := range task.InputRequests {
		if answered[key] {
			continue
		}
		response, err := c.answerInput(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("task %s asked for %s: %w", task.TaskID, request.Method, err)
		}
		responses[key] = response
	}
	return responses, nil
}

var errNoInputHandler = errors.New("this client does not handle it")

func (c *Client) answerInput(ctx context.Context, request tasks.InputRequest) (json.RawMessage, error) {
	handler := c.clientHandler
	if handler == nil || !handler.Implements(request.Method) {
		return nil, errNoInputHandler
	}
	params := request.Params
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	var (
		result interface{}
		rpcErr *jsonrpc.Error
	)
	switch request.Method {
	case schema.MethodElicitationCreate:
		typed := &schema.ElicitRequest{Method: request.Method}
		if err := json.Unmarshal(params, &typed.Params); err != nil {
			return nil, err
		}
		result, rpcErr = handler.Elicit(ctx, &jsonrpc.TypedRequest[*schema.ElicitRequest]{Method: request.Method, Request: typed})
	case schema.MethodSamplingCreateMessage:
		typed := &schema.CreateMessageRequest{Method: request.Method}
		if err := json.Unmarshal(params, &typed.Params); err != nil {
			return nil, err
		}
		result, rpcErr = handler.CreateMessage(ctx, &jsonrpc.TypedRequest[*schema.CreateMessageRequest]{Method: request.Method, Request: typed})
	case schema.MethodRootsList:
		typed := &schema.ListRootsRequest{Method: request.Method}
		if err := json.Unmarshal(params, &typed.Params); err != nil {
			return nil, err
		}
		result, rpcErr = handler.ListRoots(ctx, &jsonrpc.TypedRequest[*schema.ListRootsRequest]{Method: request.Method, Request: typed})
	default:
		return nil, errNoInputHandler
	}
	if rpcErr != nil {
		return nil, rpcErr
	}
	return json.Marshal(result)
}
