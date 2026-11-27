# MCP Apps

MCP Apps (`io.modelcontextprotocol/ui`, SEP-1865) lets a server attach an interactive HTML view to its tools. The server declares the view as a `ui://` resource of type `text/html;profile=mcp-app` and links tools to it through their `_meta.ui`. A host that supports the extension renders the view in a sandboxed iframe. The view talks to the host over `postMessage` using MCP's JSON-RPC: it receives the tool's arguments and result, and it can call the server's tools through the host. A host without the extension just calls the tool and shows its text.

The spec is [ext-apps, specification/2026-01-26/apps.mdx](https://github.com/modelcontextprotocol/ext-apps/blob/main/specification/2026-01-26/apps.mdx). [`example/apps`](../example/apps) shows both ends over Streamable HTTP; run it with `go run ./example/apps/cmd`.

## Serving an app

Register the view, then link tools to it:

```go
import (
    "github.com/eberle1080/mcp-protocol/extension/apps"
    serverproto "github.com/eberle1080/mcp-protocol/server"
)

newHandler := serverproto.WithDefaultHandler(ctx, func(h *serverproto.DefaultHandler) error {
    border := true
    err := h.RegisterAppResource(
        schema.Resource{Uri: "ui://weather/dashboard", Name: "weather_dashboard"},
        apps.ResourceMeta{
            CSP:           &apps.CSP{ConnectDomains: []string{"https://api.openweathermap.org"}},
            PrefersBorder: &border,
        },
        serverproto.StaticHTML(dashboardHTML),
    )
    if err != nil {
        return err
    }
    if err := serverproto.RegisterTool[*CityInput, *Weather](h.Registry, "get_weather", "Current weather", getWeather); err != nil {
        return err
    }
    return h.LinkToolToApp("get_weather", apps.ToolMeta{ResourceURI: "ui://weather/dashboard"})
})
```

`RegisterAppResource` does the following:
- Serves the HTML with the app content type.
- Puts the metadata both on the `resources/list` entry, where a host can review it before reading, and on every content item, where hosts must look for it.
- Declares the extension (`{}`) in `initialize` and `server/discover`.

It refuses a URI that is not `ui://`, and any CSP entry that is not a plain origin. A host copies these entries into a policy, so `"https://a.example; script-src *"` would rewrite it.

The HTML source is called on every read. `StaticHTML` serves a fixed document.

`LinkToolToApp` works with any registration method. It writes `_meta.ui` and, like the official SDK, also writes the deprecated `_meta["ui/resourceUri"]` for hosts that read only the old form. The view must already be registered. Tool-specific metadata from `MetadataFor` gets the link too.

### Who may call a tool

`ToolMeta.Visibility` takes `"model"` and `"app"`. Leaving it empty means both.

| Visibility | The model sees and calls it | The view may call it |
| --- | --- | --- |
| (empty) or `["model", "app"]` | yes | yes |
| `["model"]` | yes | no |
| `["app"]` | no | yes |

A host that renders apps enforces this. A client that does not declare the extension would show an `["app"]` tool to its model, so `DefaultHandler` hides such tools from that client's `tools/list` and answers its `tools/call` for them as for an unknown tool. In 2026-07-28 the declaration is checked on each request. Before that, it is checked on the one made at `initialize`. As a result, `tools/list` differs between clients that render apps and clients that don't, so don't mark it as publicly cacheable.

### What a tool returns

A tool must still return useful `content` for hosts that show no view and for the model. Put the data the view renders in `structuredContent`; it is not added to the model's context.

## The host side

The host's front end renders the view: a sandboxed iframe on an origin other than the host's, the policy below, and the `ui/*` messages. The Go side, which talks to the MCP server, is in `client/apphost`.

Declare the extension so the server knows views can be shown. The `mcpb` bridge passes a desktop host's declaration upstream.

```go
cli := client.New("host", "1.0", transport, client.WithExtension(apps.ExtensionID, apps.ClientSettings()))
// or, with the mcp.NewClient helper:
options.Extensions = map[string]map[string]interface{}{apps.ExtensionID: apps.ClientSettings()}
```

Then:

- **`apphost.ModelTools(tools)`** returns the tools you may give the model. It drops `["app"]` tools.
- **`apphost.LoadView(ctx, cli, tool, listing)`** reads the tool's view and checks it. The read must return exactly one item for the view's URI, of type `text/html;profile=mcp-app`, carrying text or a blob but not both. It returns:
  - the HTML;
  - the metadata, taken from the content item, or from the `resources/list` entry if you pass it and the item has none;
  - the `Content-Security-Policy` to serve the HTML under;
  - the iframe `allow` attribute for the permissions the view asked for;
  - a SHA-256 digest for allowlists and audit logs.

  CSP entries that are not valid origins are left out of the policy and listed in `RejectedOrigins`. A host may refuse to render a view that has any.
- **`apphost.NewBridge(cli, tools)`** is what you route a view's requests through.
  - It forwards `tools/call` only for tools in the server's list whose visibility includes `"app"`, and `resources/read` only to the view's own server. A bridge is bound to one server, so a view can never reach another.
  - It forwards only the tool name and arguments, or the URI. A view's own `_meta` (an authorization token, a progress token, keys the host sets) never reaches the server.
  - Refused calls wrap `apphost.ErrRefused` and never reach the server.
  - Set `Audit` to log every request a view makes, as the spec asks.

The connection you pass must declare the extension, or the server will not list view-only tools to it. A `*server.Adapter` declares none.

Metadata is read exactly as the spec spells it. Go's JSON decoding matches keys case-insensitively, so `CSP` or `ConnectDomains` would otherwise add origins that other hosts, and anyone auditing the metadata, never see. A tool whose UI metadata does not parse is shown to neither the model nor views. An explicitly empty `visibility` allows no one, as a host checking `visibility.includes(...)` reads it.

The policy is built as the spec constructs it.
- **No CSP metadata:** scripts and styles come only from the document itself, images and media also from `data:`, and there are no connections, nested frames or plugins.
- **CSP metadata:** the declared resource origins are added to the script, style, image, font and media sources, and the connect, frame and base-URI origins go to their directives. Nothing beyond what the metadata declares is ever allowed.

The view gets the tool's input and result from your front end (`ui/notifications/tool-input` and `ui/notifications/tool-result`), not from the server. See `example/apps/view.html` for a view that needs no SDK.
