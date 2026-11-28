package skills

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"time"

	"github.com/eberle1080/jsonrpc"
	skillformat "github.com/eberle1080/mcp-protocol/extension/skills"
	"github.com/eberle1080/mcp-protocol/schema"
	serverproto "github.com/eberle1080/mcp-protocol/server"
	"github.com/eberle1080/mcp/server"
)

// Each directory under skills/ is one skill; its name is the skill's name.
//
//go:embed skills
var embedded embed.FS

// Compile seals every embedded skill: it reads the files once, computes their
// digests, and keeps the bytes. Do it once, outside the handler factory. A
// 2026-07-28 request builds a fresh handler, so compiling inside the factory
// would redo this work on every request.
func Compile(ctx context.Context) ([]*skillformat.Static, error) {
	dirs, err := fs.ReadDir(embedded, "skills")
	if err != nil {
		return nil, err
	}
	var compiled []*skillformat.Static
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		source, err := fs.Sub(embedded, "skills/"+dir.Name())
		if err != nil {
			return nil, err
		}
		skill, err := (skillformat.Compiler{Source: source}).Compile(ctx, "skill://"+dir.Name()+"/SKILL.md")
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", dir.Name(), err)
		}
		compiled = append(compiled, skill)
	}
	return compiled, nil
}

// DailyReport is the URI of the generated skill.
const DailyReport = "skill://reports/daily/SKILL.md"

// NewHandler serves the compiled skills and a generated one. now supplies the
// date the generated skill reports on.
func NewHandler(compiled []*skillformat.Static, now func() time.Time) serverproto.NewHandler {
	return serverproto.WithDefaultHandler(context.Background(), func(h *serverproto.DefaultHandler) error {
		// The skills are the same for every caller and change only with a
		// new build, so clients may cache their metadata, shared, for an hour.
		h.SkillsCacheTTL = time.Hour
		h.SkillsCacheScope = schema.CacheableResultCacheScopePublic
		for _, skill := range compiled {
			if err := h.RegisterStaticSkill(skill); err != nil {
				return err
			}
		}
		return registerDailyReport(h, now)
	})
}

// registerDailyReport serves a skill whose content changes every day, so the
// server cannot publish digests for it: its entry says "dynamic". Its files
// are ordinary registered resources, all of them, so resources/directory/read
// can list them completely. Only the server's author can promise that, which
// is why a handler with a dynamic skill turns directory reads on explicitly.
func registerDailyReport(h *serverproto.DefaultHandler, now func() time.Time) error {
	everyFileRegistered := true
	h.SkillsDirectoryRead = &everyFileRegistered
	type file struct {
		name     string
		generate func() string
	}
	files := map[string]file{
		// SEP-2640: SKILL.md's resource takes the skill's name.
		DailyReport: {"daily", func() string {
			return "---\nname: daily\ndescription: Assemble today's operational report\n---\n\n" +
				"# Daily report for " + now().Format("2006-01-02") + "\n\nStart with `sections/summary.md`.\n"
		}},
		"skill://reports/daily/sections/summary.md": {"summary.md", func() string {
			return "Summarize the open incidents as of " + now().Format(time.RFC3339) + ".\n"
		}},
	}
	for uri, f := range files {
		uri, f := uri, f
		mimeType := "text/markdown"
		err := h.RegisterResource(schema.Resource{Uri: uri, Name: f.name, MimeType: &mimeType}, func(context.Context, *schema.ReadResourceRequest) (*schema.ReadResourceResult, *jsonrpc.Error) {
			return &schema.ReadResourceResult{Contents: []schema.ReadResourceResultContentsElem{{Uri: uri, MimeType: &mimeType, Text: f.generate()}}}, nil
		})
		if err != nil {
			return err
		}
	}
	// The entry's frontmatter must match what SKILL.md says; hosts compare
	// the two field by field.
	return h.RegisterSkill(schema.Skill{
		Uri:         DailyReport,
		Frontmatter: map[string]interface{}{"name": "daily", "description": "Assemble today's operational report"},
		Resources:   schema.SkillResources{Dynamic: true},
	})
}

// NewServer returns a Streamable HTTP MCP server for the skills.
func NewServer(compiled []*skillformat.Static, now func() time.Time) (*server.Server, error) {
	srv, err := server.New(
		server.WithNewHandler(NewHandler(compiled, now)),
		server.WithImplementation(schema.Implementation{Name: "skills-example", Version: "1.0"}),
	)
	if err != nil {
		return nil, err
	}
	srv.UseStreamableHTTP(true)
	return srv, nil
}
