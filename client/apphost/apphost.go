// Package apphost is the Go side of a host that renders MCP Apps
// (io.modelcontextprotocol/ui, SEP-1865): the part that talks to the MCP
// server. It
//
//   - keeps tools meant only for views out of the model's tool list
//     (ModelTools);
//   - reads a tool's view, checks it, and works out the Content Security
//     Policy and iframe allow attribute to serve it under (LoadView);
//   - forwards a view's requests to its own server, refusing the tools a view
//     may not call (Bridge).
//
// Rendering the view in a sandboxed iframe on a separate origin, and the ui/*
// messages between view and host, belong to the host's front end. A client
// declares that it renders apps with
// client.WithExtension(apps.ExtensionID, apps.ClientSettings()).
package apphost

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/eberle1080/mcp-protocol/extension/apps"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/eberle1080/mcp/client"
)

// Source is one server connection. *client.Client and the client pool's
// managed clients satisfy it, and must declare MCP Apps
// (client.WithExtension(apps.ExtensionID, apps.ClientSettings())) for the
// server to list view-only tools. A *server.Adapter also satisfies it but
// declares no extensions, so the server hides view-only tools from it.
type Source interface {
	ReadResource(ctx context.Context, params *schema.ReadResourceRequestParams, options ...client.RequestOption) (*schema.ReadResourceResult, error)
	CallTool(ctx context.Context, params *schema.CallToolRequestParams, options ...client.RequestOption) (*schema.CallToolResult, error)
}

// ModelTools returns the tools a host may show the model: every tool whose
// visibility includes "model", which is every tool that sets none.
func ModelTools(tools []schema.Tool) []schema.Tool {
	var result []schema.Tool
	for _, tool := range tools {
		if apps.VisibleToModel(tool) {
			result = append(result, tool)
		}
	}
	return result
}

// View is a tool's view, read from its server and checked, with what the host
// needs to serve it.
type View struct {
	// URI is the ui:// resource the view came from.
	URI string
	// HTML is the document to load in the sandboxed iframe.
	HTML []byte
	// Meta is the view's metadata: the content item's _meta.ui, or the
	// resources/list entry's when the item has none.
	Meta apps.ResourceMeta
	// CSP is the Content-Security-Policy to serve HTML with. It allows nothing
	// beyond what Meta declares.
	CSP string
	// RejectedOrigins are CSP entries left out of CSP because they are not
	// valid origins. A host may refuse to render a view that has any.
	RejectedOrigins []string
	// Allow is the iframe allow attribute for the permissions the view asks
	// for. Grant only what you are willing to.
	Allow string
	// Digest is "sha256:<hex>" of HTML, for allowlists and audit logs.
	Digest string
}

// LoadView reads the view linked to tool from the tool's server. listing is
// the view's resources/list entry if the host has it (nil otherwise); its
// metadata applies when the content item carries none. The read must return
// exactly one content item for the view's URI, of type
// text/html;profile=mcp-app, holding text or a blob.
func LoadView(ctx context.Context, source Source, tool schema.Tool, listing *schema.Resource, options ...client.RequestOption) (*View, error) {
	meta, linked := apps.ToolMetaOf(tool)
	if !linked || meta.ResourceURI == "" {
		return nil, fmt.Errorf("tool %q has no view", tool.Name)
	}
	uri := meta.ResourceURI
	if err := apps.ValidateResourceURI(uri); err != nil {
		return nil, fmt.Errorf("tool %q: %w", tool.Name, err)
	}
	result, err := source.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: uri}, options...)
	if err != nil {
		return nil, err
	}
	item, err := viewItem(uri, result)
	if err != nil {
		return nil, err
	}
	var html []byte
	if item.Blob != "" {
		if html, err = base64.StdEncoding.DecodeString(item.Blob); err != nil {
			return nil, fmt.Errorf("view %s: invalid base64: %w", uri, err)
		}
	} else {
		html = []byte(item.Text)
	}
	resourceMeta, found, err := apps.ResourceMetaOf(item.Meta)
	if err != nil {
		return nil, fmt.Errorf("view %s: %w", uri, err)
	}
	if !found && listing != nil && listing.Uri == uri {
		if resourceMeta, _, err = apps.ResourceMetaOf(listing.Meta); err != nil {
			return nil, fmt.Errorf("view %s listing: %w", uri, err)
		}
	}
	policy, rejected := resourceMeta.ContentSecurityPolicy()
	return &View{
		URI:             uri,
		HTML:            html,
		Meta:            resourceMeta,
		CSP:             policy,
		RejectedOrigins: rejected,
		Allow:           resourceMeta.AllowAttribute(),
		Digest:          fmt.Sprintf("sha256:%x", sha256.Sum256(html)),
	}, nil
}

func viewItem(uri string, result *schema.ReadResourceResult) (schema.ReadResourceResultContentsElem, error) {
	if result == nil {
		return schema.ReadResourceResultContentsElem{}, fmt.Errorf("view %s: the read returned no result", uri)
	}
	var match *schema.ReadResourceResultContentsElem
	count := 0
	for i := range result.Contents {
		if result.Contents[i].Uri == uri {
			match = &result.Contents[i]
			count++
		}
	}
	switch {
	case count != 1:
		return schema.ReadResourceResultContentsElem{}, fmt.Errorf("view %s: the read returned %d content items for it, want 1", uri, count)
	case match.MimeType == nil || *match.MimeType != apps.MimeType:
		got := "none"
		if match.MimeType != nil {
			got = *match.MimeType
		}
		return schema.ReadResourceResultContentsElem{}, fmt.Errorf("view %s has content type %s, want %s", uri, got, apps.MimeType)
	case match.Text != "" && match.Blob != "":
		return schema.ReadResourceResultContentsElem{}, fmt.Errorf("view %s carries both text and a blob", uri)
	case match.Text == "" && match.Blob == "":
		return schema.ReadResourceResultContentsElem{}, fmt.Errorf("view %s is empty", uri)
	}
	return *match, nil
}

// ErrRefused is wrapped by every request a Bridge refuses.
var ErrRefused = errors.New("refused for a view")

// Bridge forwards a view's requests to the view's own server. A host gives
// each rendered view a Bridge for the server the view came from, so a view can
// never reach another server.
type Bridge struct {
	source Source
	tools  map[string]schema.Tool
	// Audit, if set, sees every request a view makes, before it is forwarded
	// or refused. SEP-1865 asks hosts to log them for security review.
	Audit func(ctx context.Context, method, name string, refused error)
}

// NewBridge returns a bridge to source. tools is the server's tools/list as
// the host received it: a view may call only the tools in it whose visibility
// includes "app".
func NewBridge(source Source, tools []schema.Tool) *Bridge {
	known := make(map[string]schema.Tool, len(tools))
	for _, tool := range tools {
		known[tool.Name] = tool
	}
	return &Bridge{source: source, tools: known}
}

// CallTool forwards a view's tools/call. It refuses a tool the server did not
// list and a tool whose visibility leaves out "app".
func (b *Bridge) CallTool(ctx context.Context, params *schema.CallToolRequestParams, options ...client.RequestOption) (*schema.CallToolResult, error) {
	if params == nil {
		return nil, fmt.Errorf("%w: tools/call without parameters", ErrRefused)
	}
	tool, known := b.tools[params.Name]
	var refused error
	switch {
	case !known:
		refused = fmt.Errorf("%w: tool %q is not one of this server's tools", ErrRefused, params.Name)
	case !apps.CallableByApp(tool):
		refused = fmt.Errorf("%w: tool %q is not callable by views", ErrRefused, params.Name)
	}
	if b.Audit != nil {
		b.Audit(ctx, schema.MethodToolsCall, params.Name, refused)
	}
	if refused != nil {
		return nil, refused
	}
	// Forward only what a view may decide: the tool and its arguments. The
	// view's own _meta (an authorization token, a progress token, metadata
	// the host sets) never reaches the server.
	return b.source.CallTool(ctx, &schema.CallToolRequestParams{Name: params.Name, Arguments: params.Arguments}, options...)
}

// ReadResource forwards a view's resources/read to the view's server.
func (b *Bridge) ReadResource(ctx context.Context, params *schema.ReadResourceRequestParams, options ...client.RequestOption) (*schema.ReadResourceResult, error) {
	if params == nil {
		return nil, fmt.Errorf("%w: resources/read without parameters", ErrRefused)
	}
	if b.Audit != nil {
		b.Audit(ctx, schema.MethodResourcesRead, params.Uri, nil)
	}
	return b.source.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: params.Uri}, options...)
}
