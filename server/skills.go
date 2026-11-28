package server

import (
	"context"
	"encoding/json"
	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/schema"
	protocol "github.com/eberle1080/mcp-protocol/server"
)

func (h *Handler) ListSkills(ctx context.Context, request *jsonrpc.Request) (*schema.ListSkillsResult, *jsonrpc.Error) {
	handler, ok := h.handler.(protocol.Skills)
	if !ok {
		return nil, jsonrpc.NewMethodNotFound("skills/list is unavailable", nil)
	}
	params := &schema.ListSkillsRequest{Method: schema.MethodSkillsList}
	if err := unmarshalOptionalParams(request.Params, &params.Params); err != nil {
		return nil, jsonrpc.NewInvalidParamsError(err.Error(), nil)
	}
	id, _ := jsonrpc.AsRequestIntId(request.Id)
	return handler.ListSkills(ctx, &jsonrpc.TypedRequest[*schema.ListSkillsRequest]{Id: uint64(id), Method: request.Method, Request: params})
}

func (h *Handler) GetSkill(ctx context.Context, request *jsonrpc.Request) (*schema.GetSkillResult, *jsonrpc.Error) {
	handler, ok := h.handler.(protocol.Skills)
	if !ok {
		return nil, jsonrpc.NewMethodNotFound("skills/get is unavailable", nil)
	}
	params := &schema.GetSkillRequest{Method: schema.MethodSkillsGet}
	if err := json.Unmarshal(request.Params, &params.Params); err != nil {
		return nil, jsonrpc.NewInvalidParamsError(err.Error(), nil)
	}
	if params.Params.Uri == "" {
		return nil, jsonrpc.NewInvalidParamsError("skill URI is required", nil)
	}
	id, _ := jsonrpc.AsRequestIntId(request.Id)
	return handler.GetSkill(ctx, &jsonrpc.TypedRequest[*schema.GetSkillRequest]{Id: uint64(id), Method: request.Method, Request: params})
}

// ReadResourceDirectory dispatches SEP-2640's resources/directory/read to a
// handler that implements protocol.ResourceDirectories.
func (h *Handler) ReadResourceDirectory(ctx context.Context, request *jsonrpc.Request) (*schema.ReadResourceDirectoryResult, *jsonrpc.Error) {
	handler, ok := h.handler.(protocol.ResourceDirectories)
	if !ok {
		return nil, jsonrpc.NewMethodNotFound("resources/directory/read is unavailable", nil)
	}
	params := &schema.ReadResourceDirectoryRequest{Method: schema.MethodResourcesDirectoryRead}
	if err := json.Unmarshal(request.Params, &params.Params); err != nil {
		return nil, jsonrpc.NewInvalidParamsError(err.Error(), nil)
	}
	if params.Params.Uri == "" {
		return nil, jsonrpc.NewInvalidParamsError("directory URI is required", nil)
	}
	id, _ := jsonrpc.AsRequestIntId(request.Id)
	return handler.ReadResourceDirectory(ctx, &jsonrpc.TypedRequest[*schema.ReadResourceDirectoryRequest]{Id: uint64(id), Method: request.Method, Request: params})
}

// withdrawUnservedSettings drops skills declarations that dispatch could not
// honour. A handler that forwards Initialize to a DefaultHandler without
// embedding it declares what the DefaultHandler serves, but dispatch needs the
// methods on the handler itself. The extension forbids declaring methods the
// server does not implement: without ListSkills/GetSkill the extension is
// withdrawn, and without ReadResourceDirectory its directoryRead setting.
func (h *Handler) withdrawUnservedSettings(capabilities *schema.ServerCapabilities) {
	settings, ok := capabilities.Extensions[schema.SkillsExtension]
	if !ok {
		return
	}
	_, servesSkills := h.handler.(protocol.Skills)
	_, servesDirectories := h.handler.(protocol.ResourceDirectories)
	if servesSkills && (servesDirectories || settings[schema.SkillsDirectoryRead] != true) {
		return
	}
	// Copy rather than edit: the maps may be shared with the handler.
	extensions := make(map[string]map[string]interface{}, len(capabilities.Extensions))
	for key, value := range capabilities.Extensions {
		if key != schema.SkillsExtension {
			extensions[key] = value
		}
	}
	if servesSkills {
		withdrawn := make(map[string]interface{}, len(settings))
		for key, value := range settings {
			if key != schema.SkillsDirectoryRead {
				withdrawn[key] = value
			}
		}
		extensions[schema.SkillsExtension] = withdrawn
	}
	capabilities.Extensions = extensions
}
