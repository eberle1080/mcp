package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/authorization"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/eberle1080/gosh"
)

type TerminalCommand struct {
	Commands []string          `json:"commands"`
	Evn      map[string]string `json:"evn"`
}

type TerminalTool struct {
	service *gosh.Service
}

type CommandOutput struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
}

func (t *TerminalTool) Call(ctx context.Context, input *TerminalCommand) (*schema.CallToolResult, *jsonrpc.Error) {

	token := ctx.Value(authorization.TokenKey)
	if token != nil {
		fmt.Printf("token: %+v\n", token)
	}
	// Convert commands to a single string command
	cmdString := ""
	if len(input.Commands) > 0 {
		cmdString = input.Commands[0]
		for i := 1; i < len(input.Commands); i++ {
			cmdString += " && " + input.Commands[i]
		}
	}

	result := &schema.CallToolResult{}
	output, code, err := t.service.Run(ctx, cmdString)
	if err != nil {
		return nil, jsonrpc.NewInternalError(err.Error(), []byte(cmdString))
	}

	cmdOutput := &CommandOutput{
		Stdout: output,
		Code:   code,
	}

	if code != 0 {
		isError := true
		result.IsError = &isError
	}

	data, err := json.Marshal(cmdOutput)
	if err != nil {
		return nil, jsonrpc.NewInternalError(err.Error(), []byte(cmdString))
	}
	result.Content = []schema.CallToolResultContentElem{
		schema.TextContent{Text: string(data), Type: "text"},
	}
	return result, nil
}

func NewTool(service *gosh.Service) *TerminalTool {
	return &TerminalTool{
		service: service,
	}
}
