package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/eberle1080/mcp/client"
)

// Walkthrough builds the report three times and writes what it sees to out:
//
//  1. With CallTool, which waits for the task and answers its question with
//     the client's handler (User below), as it would a direct elicitation.
//  2. With CallToolTask, polling GetTask, answering with UpdateTask.
//  3. Starting a task and cancelling it.
func Walkthrough(ctx context.Context, cli *client.Client, out io.Writer) error {
	result, err := cli.CallTool(ctx, &schema.CallToolRequestParams{Name: "build_report", Arguments: map[string]interface{}{"quarter": "2026-Q3"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "CallTool: %s\n", text(result))

	_, task, err := cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "build_report", Arguments: map[string]interface{}{"quarter": "2026-Q2"}})
	if err != nil {
		return err
	}
	if task == nil {
		return errors.New("the server did not answer with a task")
	}
	fmt.Fprintf(out, "task created: %s (%s)\n", task.Status, task.StatusMessage)
	seen := map[string]bool{}
	for !task.Status.Terminal() {
		if !seen[task.StatusMessage] && task.StatusMessage != "" {
			seen[task.StatusMessage] = true
			fmt.Fprintf(out, "task %s: %s\n", task.Status, task.StatusMessage)
		}
		if task.Status == tasks.InputRequired {
			for key, request := range task.InputRequests {
				var params schema.ElicitRequestParams
				_ = json.Unmarshal(request.Params, &params)
				fmt.Fprintf(out, "task asks (%s): %s -> no\n", request.Method, params.Message)
				answer, _ := json.Marshal(schema.ElicitResult{Action: schema.ElicitResultActionAccept, Content: map[string]interface{}{"include": false}})
				if err := cli.UpdateTask(ctx, task.TaskID, map[string]json.RawMessage{key: answer}); err != nil {
					return err
				}
			}
		}
		time.Sleep(time.Duration(*task.PollIntervalMs) * time.Millisecond)
		if task, err = cli.GetTask(ctx, task.TaskID); err != nil {
			return err
		}
	}
	report, err := cli.AwaitTask(ctx, *task) // decodes the finished task's result
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "task %s: %s\n", task.Status, text(report))

	_, task, err = cli.CallToolTask(ctx, &schema.CallToolRequestParams{Name: "build_report", Arguments: map[string]interface{}{"quarter": "2026-Q1"}})
	if err != nil {
		return err
	}
	if err := cli.CancelTask(ctx, task.TaskID); err != nil {
		return err
	}
	_, err = cli.AwaitTask(ctx, *task)
	var taskErr *client.TaskError
	if !errors.As(err, &taskErr) {
		return fmt.Errorf("expected a cancelled task, got %v", err)
	}
	fmt.Fprintf(out, "cancelled: %v\n", taskErr)
	return nil
}

func text(result *schema.CallToolResult) string {
	var parts []string
	for _, item := range result.Content {
		switch content := item.(type) {
		case schema.TextContent:
			parts = append(parts, content.Text)
		case map[string]interface{}:
			if t, ok := content["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, " ")
}

// User is the client's handler: it answers elicitations as a user would, by
// including unaudited figures. Input requests that arrive through a task get
// the same treatment as direct requests.
type User struct{}

func (User) Notify(context.Context, *jsonrpc.Notification) error   { return nil }
func (User) NextRequestID() jsonrpc.RequestId                      { return 0 }
func (User) LastRequestID() jsonrpc.RequestId                      { return 0 }
func (User) Init(context.Context, *schema.ClientCapabilities)      {}
func (User) OnNotification(context.Context, *jsonrpc.Notification) {}
func (User) Implements(method string) bool                         { return method == schema.MethodElicitationCreate }
func (User) ListRoots(context.Context, *jsonrpc.TypedRequest[*schema.ListRootsRequest]) (*schema.ListRootsResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("roots/list", nil)
}
func (User) CreateMessage(context.Context, *jsonrpc.TypedRequest[*schema.CreateMessageRequest]) (*schema.CreateMessageResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("sampling/createMessage", nil)
}
func (User) Elicit(context.Context, *jsonrpc.TypedRequest[*schema.ElicitRequest]) (*schema.ElicitResult, *jsonrpc.Error) {
	return &schema.ElicitResult{Action: schema.ElicitResultActionAccept, Content: map[string]interface{}{"include": true}}, nil
}
