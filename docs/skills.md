# Skills

The skills extension (`io.modelcontextprotocol/skills`, SEP-2640) lets a server serve [Agent Skills](https://agentskills.io/): directories of instructions, minimally a `SKILL.md`, that an agent loads when a task calls for them. Each file of a skill is an ordinary MCP resource, usually under `skill://<skill-path>/<file>`, and is read with `resources/read`. The extension adds three methods:

- `skills/list`: the skills the server serves. Each entry carries the skill's frontmatter and, unless the skill is generated, every file's size and SHA-256 digest.
- `skills/get`: one skill's entry, by the URI of its `SKILL.md`.
- `resources/directory/read` (optional): the direct children of a directory, such as a skill's `templates/`.

The spec is [ext-skills, specification/beta/skills.mdx](https://github.com/modelcontextprotocol/ext-skills/blob/main/specification/beta/skills.mdx). [`example/skills`](../example/skills) shows both ends working over Streamable HTTP; run it with `go run ./example/skills/cmd`.

## Serving skills

### A skill made of files

Compile the skill once, when the process starts, and register the compiled value in the handler factory:

```go
import (
    skillformat "github.com/eberle1080/mcp-protocol/extension/skills"
    serverproto "github.com/eberle1080/mcp-protocol/server"
)

//go:embed skills/pdf-processing
var files embed.FS

source, _ := fs.Sub(files, "skills/pdf-processing")
compiled, err := skillformat.Compiler{Source: source}.Compile(ctx, "skill://pdf-processing/SKILL.md")

newHandler := serverproto.WithDefaultHandler(ctx, func(h *serverproto.DefaultHandler) error {
    return h.RegisterStaticSkill(compiled)
})
```

`Compile` reads every file once, checks the frontmatter against the Agent Skills rules, enforces the limits (512 files, 16 MiB), and computes the digests. The compiled value keeps the bytes. Later changes to the source do not reach clients.

Compile outside the factory. A 2026-07-28 request has no session, so the server builds a fresh handler for every request, and compiling inside the factory would repeat the work each time.

The last segment of the skill's path must be its `name`. `skill://acme/billing/refunds/SKILL.md` is a skill named `refunds`.

`RegisterStaticSkill` registers each file as a resource. It also serves `skills/list`, `skills/get` and `resources/directory/read`, and declares the extension (and the `resources` capability) in `initialize` and `server/discover`. No ordinary resource can be added inside the skill afterwards.

### A generated skill

When a skill's content is generated, the server cannot publish digests. Register each file as an ordinary resource, then register the entry with `"dynamic"` resources:

```go
h.RegisterResource(schema.Resource{Uri: "skill://reports/daily/SKILL.md", Name: "daily"}, readSkillMD)
h.RegisterResource(schema.Resource{Uri: "skill://reports/daily/sections/summary.md", Name: "summary.md"}, readSummary)
h.RegisterSkill(schema.Skill{
    Uri:         "skill://reports/daily/SKILL.md",
    Frontmatter: map[string]interface{}{"name": "daily", "description": "Assemble today's operational report"},
    Resources:   schema.SkillResources{Dynamic: true},
})
```

The `Frontmatter` must be exactly what the generated `SKILL.md` says. Hosts compare the two field by field and refuse the skill when they differ. Some hosts refuse dynamic skills altogether, because nothing about them can be verified.

Registering a dynamic skill turns directory reads off unless you turn them on (see below).

### Cache hints

`skills/list`, `skills/get` and `resources/directory/read` results carry `ttlMs` and `cacheScope` in 2026-07-28. By default they are 0 ms and `private`, so clients fetch again each time. A server whose skills are the same for every caller can say so:

```go
h.SkillsCacheTTL = time.Hour
h.SkillsCacheScope = schema.CacheableResultCacheScopePublic
```

Results for older protocol versions carry neither field.

### Directory reads

The default implementation lists a directory's children from the resources registered under it. That covers a compiled skill completely, so `resources/directory/read` is on, and the extension declared as `{"directoryRead": true}`, while every registered skill is compiled. A generated skill's files are listed only if they are registered resources, which only you can promise, so registering a dynamic skill turns directory reads off. Set `SkillsDirectoryRead` to say otherwise:

```go
on := true // every file of every dynamic skill is a registered resource
h.SkillsDirectoryRead = &on
```

If you serve a generated skill's files from a resource template, override `ReadResourceDirectory` on your handler instead. Setting `SkillsDirectoryRead` to `false` turns directory reads off entirely.

The server never declares what it cannot dispatch. If your handler declares the extension (for example by forwarding `Initialize` to a `DefaultHandler`) but has no `ListSkills`/`GetSkill` methods of its own, the declaration is withdrawn. If it has no `ReadResourceDirectory`, the `directoryRead` setting is withdrawn.

A directory URI has no trailing slash (`skill://pdf-processing/templates`). Subdirectories come back with `mimeType: "inode/directory"`. Asking for a file, or for anything that is not a directory inside a registered skill, returns `-32602`.

### `_meta` on entries

An entry can carry a `_meta` object: `Compiler.Meta` for a compiled skill, `Skill.Meta` for a generated one. The `io.modelcontextprotocol.skills/` prefix is reserved for the extension; use your own reverse-domain prefix.

### Authorization

Rules in `authorization.Policy.Resources` that protect skill files also protect the skills methods:

| Method | Needs |
| --- | --- |
| `skills/list` | every resource rule |
| `skills/get` | the rules for files under that skill |
| `resources/directory/read` | the rules for files under that directory |
| `resources/read` | the rule for that file (only with `RequireResourceAuthorization`) |

The first three are enforced whether or not `RequireResourceAuthorization` is set, because their results name protected files. `AuthorizeResource` decides whether a credential satisfies a rule.

## Using skills from a client

`*client.Client`, `*server.Adapter` and the client pool's managed clients have `ListSkills`, `GetSkill` and `ReadResourceDirectory`. On a `*client.Client`, `ReadResourceDirectory` fails without sending anything when the server has not declared `directoryRead`, as the extension requires. `client.DirectoryReadDeclared(capabilities)` tells you in advance. `*server.Adapter` calls its in-process handler directly, which answers `-32601` when directory reads are off.

### Loading a skill: `client/skillhost`

`skillhost` loads a skill and checks everything it reads:

```go
import "github.com/eberle1080/mcp/client/skillhost"

skill, err := skillhost.Load(ctx, cli, "skill://pdf-processing/SKILL.md") // or LoadEntry with a listed entry
instructions := skill.SkillMD()
children, err := skill.List(ctx, "templates")
invoice, err := skill.Read(ctx, "templates/invoice.md")
```

It enforces the extension's requirements on reads:

- **Nothing is fetched ahead of need.** Loading reads `SKILL.md` and nothing else; a supporting file is read when you ask for it.
- **Reads stay with the skill's server.** A `Skill` is bound to the connection it was loaded through. It reads only URIs under the skill root that are spelled canonically: no dot segments, escaped slashes, queries or fragments, so a server that normalizes URIs cannot be led outside the skill.
- **Only listed files are read.** For a skill whose entry lists its files, a file the entry does not list fails without a request.
- **Bytes are checked.** A file is returned only if its size and SHA-256 digest match the entry. `SKILL.md`'s frontmatter must also equal the entry's.
- **Listings come from the entry.** For a listed skill, `List` answers from the entry, so it never shows a file the entry does not have. For a generated skill it calls `resources/directory/read`.
- **Generated skills are bounded.** A dynamic skill may deliver at most 16 MiB in all.

A failed check returns a `*skills.VerificationError` (`skills.IsVerificationError(err)`) and no content. The content may be stale rather than malicious. Recover by fetching the entry again with `skills/get`. If its files changed, the user's approval no longer applies.

References are read the way a Markdown link is: `#` starts a fragment and `?` a query (which is refused). A file whose name contains `#`, `?` or `%` is written with escapes (`notes%23v2.md`). A name with `:` before any `/` is written `./a:b.md`. The compiler refuses file names containing `%` or `\`, which a host could not load.

The client pool's `ReadSkillResource(ctx, manifest, uri)` applies the same checks. It decides whether a URI belongs to the skill however the URI is spelled, so `SKILL://…` or `skill://…/a/../b` cannot pass for a read outside it. It returns, and keeps, only the content item it checked. A content item that carries both `text` and `blob` is refused.

### What your application still has to do

The extension puts these on the host, and no library can do them for you:

- **Ask before loading.** If you persist a user's approval of a skill, store it with `skills.ContentBinding(entry)`. Treat it as revoked when a later entry's binding differs. A dynamic skill has no binding, so an approval cannot cover whatever it serves next.
- **Show where a skill came from.** Tag skill content with the server it came from when it enters the model's context. Use a label you assign, not the server's self-reported name, and never present it as a local skill.
- **Keep same-named skills apart.** A skill's `name` is a label. Two servers, or two paths on one server, can serve skills with the same name. Resolve names per server, never let an MCP skill shadow a local one, and identify a skill by (your server label, URI).
- **Ignore `allowed-tools`** for MCP skills unless the user approved that grant for that skill. Gate any code execution a skill asks for behind the user's approval.
- **Ask again for nested skills.** Approving a skill does not approve a `SKILL.md` nested inside it.
- **Cache safely.** If you cache skill files on disk, keep the cache where only your host can write, or recompute digests on every use. Keep it outside every path you load local skills from.
