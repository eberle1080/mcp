// SPDX-License-Identifier: Apache-2.0
// Forge resource client pooling extracted from Viant Agently Core.
package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/eberle1080/mcp-protocol/schema"
	mcpclient "github.com/eberle1080/mcp/client"
)

// DirectoryClient is optional: clients that can call SEP-2640's
// resources/directory/read.
type DirectoryClient interface {
	ReadResourceDirectory(context.Context, string, *string, ...mcpclient.RequestOption) (*schema.ReadResourceDirectoryResult, error)
}

func (c *managedClient) ReadResourceDirectory(ctx context.Context, uri string, cursor *string, opts ...mcpclient.RequestOption) (*schema.ReadResourceDirectoryResult, error) {
	client, release, err := c.use()
	if err != nil {
		return nil, err
	}
	defer release()
	d, ok := client.(DirectoryClient)
	if !ok {
		return nil, fmt.Errorf("MCP client does not support resources/directory/read")
	}
	return d.ReadResourceDirectory(ctx, uri, cursor, opts...)
}

// SkillsClient is optional; older MCP client implementations remain usable.
type SkillsClient interface {
	ListSkills(context.Context, *string, ...mcpclient.RequestOption) (*schema.ListSkillsResult, error)
	GetSkill(context.Context, string, ...mcpclient.RequestOption) (*schema.GetSkillResult, error)
}

func (c *managedClient) ListSkills(ctx context.Context, cursor *string, opts ...mcpclient.RequestOption) (*schema.ListSkillsResult, error) {
	client, release, err := c.use()
	if err != nil {
		return nil, err
	}
	defer release()
	s, ok := client.(SkillsClient)
	if !ok {
		return nil, fmt.Errorf("MCP client does not support skills extension")
	}
	return s.ListSkills(ctx, cursor, opts...)
}
func (c *managedClient) GetSkill(ctx context.Context, uri string, opts ...mcpclient.RequestOption) (*schema.GetSkillResult, error) {
	client, release, err := c.use()
	if err != nil {
		return nil, err
	}
	defer release()
	s, ok := client.(SkillsClient)
	if !ok {
		return nil, fmt.Errorf("MCP client does not support skills extension")
	}
	raw, err := c.skillMetadata(ctx, uri+"\x00manifest", func(ctx context.Context) (json.RawMessage, *int, error) {
		result, err := s.GetSkill(ctx, uri, opts...)
		if err != nil {
			return nil, nil, err
		}
		if result == nil {
			return nil, nil, fmt.Errorf("empty skill response")
		}
		ttl := result.TtlMs
		if result.ResultType != "complete" || result.Skill.Resources.Dynamic {
			zero := 0
			ttl = &zero
		}
		raw, err := json.Marshal(result)
		return raw, ttl, err
	}, opts...)
	if err != nil {
		return nil, err
	}
	var result schema.GetSkillResult
	err = json.Unmarshal(raw, &result)
	return &result, err
}
