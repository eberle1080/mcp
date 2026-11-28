package client

import (
	"context"
	"fmt"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/schema"
	authcfg "github.com/eberle1080/mcp/client/auth/config"
)

// ListSkills retrieves metadata only. It never prefetches skill content.
func (c *Client) ListSkills(ctx context.Context, cursor *string, options ...RequestOption) (*schema.ListSkillsResult, error) {
	return send[schema.ListSkillsRequestParams, schema.ListSkillsResult](ctx, c, schema.MethodSkillsList, &schema.ListSkillsRequestParams{Cursor: cursor}, options...)
}

func (c *Client) GetSkill(ctx context.Context, uri string, options ...RequestOption) (*schema.GetSkillResult, error) {
	return send[schema.GetSkillRequestParams, schema.GetSkillResult](ctx, c, schema.MethodSkillsGet, &schema.GetSkillRequestParams{Uri: uri}, options...)
}

// ReadResourceDirectory lists a directory resource's direct children with
// SEP-2640's resources/directory/read. The extension forbids calling it on a
// server that has not declared directoryRead, so without that declaration the
// call fails here and nothing is sent.
func (c *Client) ReadResourceDirectory(ctx context.Context, uri string, cursor *string, options ...RequestOption) (*schema.ReadResourceDirectoryResult, error) {
	if !c.isInitialized() {
		if err := c.ensureInitialized(ctx); err != nil {
			if authcfg.IsLinkRequired(err) {
				return nil, err
			}
			return nil, jsonrpc.NewInternalError(err.Error(), nil)
		}
	}
	c.stateMu.RLock()
	var capabilities *schema.ServerCapabilities
	if c.negotiated != nil {
		capabilities = &c.negotiated.Capabilities
	}
	declared := DirectoryReadDeclared(capabilities)
	c.stateMu.RUnlock()
	if !declared {
		return nil, fmt.Errorf("server has not declared %s in its %s settings; %s is not available", schema.SkillsDirectoryRead, schema.SkillsExtension, schema.MethodResourcesDirectoryRead)
	}
	return send[schema.ReadResourceDirectoryRequestParams, schema.ReadResourceDirectoryResult](ctx, c, schema.MethodResourcesDirectoryRead, &schema.ReadResourceDirectoryRequestParams{Uri: uri, Cursor: cursor}, options...)
}

// DirectoryReadDeclared reports whether server capabilities declare
// resources/directory/read: the skills extension with directoryRead: true.
func DirectoryReadDeclared(capabilities *schema.ServerCapabilities) bool {
	if capabilities == nil {
		return false
	}
	settings, ok := capabilities.Extensions[schema.SkillsExtension]
	if !ok {
		return false
	}
	declared, _ := settings[schema.SkillsDirectoryRead].(bool)
	return declared
}
