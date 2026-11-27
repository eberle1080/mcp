package tasks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/extension/tasks"
	"github.com/eberle1080/mcp-protocol/schema"
	serverproto "github.com/eberle1080/mcp-protocol/server"
	"github.com/eberle1080/mcp/server"
)

// ReportInput names the report to build.
type ReportInput struct {
	Quarter string `json:"quarter" description:"Quarter to report on, such as 2026-Q3"`
}

// NewHandler serves build_report. step is how long each of its three stages
// takes.
func NewHandler(step time.Duration) serverproto.NewHandler {
	return serverproto.WithDefaultHandler(context.Background(), func(h *serverproto.DefaultHandler) error {
		return serverproto.RegisterTool[*ReportInput, struct{}](h.Registry, "build_report", "Build the quarterly report", func(ctx context.Context, input *ReportInput) (*schema.CallToolResult, *jsonrpc.Error) {
			if input == nil || input.Quarter == "" {
				return nil, jsonrpc.NewInvalidParamsError("quarter is required", nil)
			}
			work := func(ctx context.Context, task tasks.Control) (*schema.CallToolResult, *jsonrpc.Error) {
				return buildReport(ctx, task, input.Quarter, step)
			}
			// Answer with a task when this client allows it.
			_, err := tasks.Start(ctx, work, tasks.WithStatusMessage("queued"), tasks.WithPollInterval(step/2))
			switch {
			case err == nil:
				return nil, nil // the client gets the task, not this return
			case errors.Is(err, tasks.ErrUnavailable):
				return buildReport(ctx, nil, input.Quarter, step) // no task: build it now, without asking
			default:
				return nil, jsonrpc.NewInternalError(err.Error(), nil)
			}
		})
	})
}

// buildReport does the work. With a task it reports progress and asks the
// user before including unaudited figures; without one it leaves them out.
func buildReport(ctx context.Context, task tasks.Control, quarter string, step time.Duration) (*schema.CallToolResult, *jsonrpc.Error) {
	stage := func(message string) *jsonrpc.Error {
		if task != nil {
			task.Progress(message)
		}
		select {
		case <-time.After(step):
			return nil
		case <-ctx.Done():
			return jsonrpc.NewInternalError("report cancelled", nil)
		}
	}
	if err := stage("collecting " + quarter + " figures"); err != nil {
		return nil, err
	}
	includeUnaudited := false
	if task != nil {
		answer, err := task.Elicit(ctx, &schema.ElicitRequestParams{
			Message: "Some " + quarter + " figures are not audited yet. Include them?",
			RequestedSchema: schema.ElicitRequestParamsRequestedSchema{
				Type:       "object",
				Properties: map[string]interface{}{"include": map[string]interface{}{"type": "boolean"}},
				Required:   []string{"include"},
			},
		})
		if err != nil {
			return nil, jsonrpc.NewInternalError(err.Error(), nil)
		}
		includeUnaudited = answer.Action == schema.ElicitResultActionAccept && answer.Content["include"] == true
	}
	if err := stage("writing the report"); err != nil {
		return nil, err
	}
	figures := "audited figures only"
	if includeUnaudited {
		figures = "audited and unaudited figures"
	}
	return &schema.CallToolResult{Content: []schema.CallToolResultContentElem{
		schema.TextContent{Type: "text", Text: fmt.Sprintf("Report for %s, with %s.", quarter, figures)},
	}}, nil
}

// NewServer returns a Streamable HTTP MCP server with tasks enabled.
func NewServer(step time.Duration) (*server.Server, error) {
	srv, err := server.New(
		server.WithNewHandler(NewHandler(step)),
		server.WithImplementation(schema.Implementation{Name: "reports", Version: "1.0"}),
		server.WithTasks(server.WithTaskTTL(time.Hour)),
	)
	if err != nil {
		return nil, err
	}
	srv.UseStreamableHTTP(true)
	return srv, nil
}
