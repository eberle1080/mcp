package skills

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/eberle1080/jsonrpc"
	pclient "github.com/eberle1080/mcp-protocol/client"
	skillformat "github.com/eberle1080/mcp-protocol/extension/skills"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/eberle1080/mcp/client"
	"github.com/eberle1080/mcp/client/skillhost"
)

// Walkthrough does what a host does with a server's skills, and writes what it
// sees to out:
//
//  1. Lists the skills. Entries carry each skill's frontmatter and, unless the
//     skill is dynamic, every file's digest, so nothing else is fetched yet.
//  2. Loads one: skillhost reads SKILL.md and checks its size, digest and
//     frontmatter against the entry. An application would ask the user first
//     and bind the approval to skills.ContentBinding(entry).
//  3. Lists and reads the files SKILL.md points to. Each read is checked; a
//     listed skill's directories come from its entry, not the server.
//  4. Does the same for the dynamic skill, whose directory listing comes from
//     resources/directory/read and whose files cannot be verified.
//  5. Shows that a file the entry does not list is refused without a read.
func Walkthrough(ctx context.Context, cli *client.Client, out io.Writer) error {
	var entries []schema.Skill
	var cursor *string
	for {
		page, err := cli.ListSkills(ctx, cursor)
		if err != nil {
			return err
		}
		entries = append(entries, page.Skills...)
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	for _, entry := range entries {
		files := "dynamic"
		if !entry.Resources.Dynamic {
			files = fmt.Sprintf("%d files", len(entry.Resources.Files))
		}
		fmt.Fprintf(out, "skill %s (%s): %s\n", entry.Uri, files, entry.Frontmatter["description"])
	}

	pdf, err := findEntry(entries, "skill://pdf-processing/SKILL.md")
	if err != nil {
		return err
	}
	binding, _ := skillformat.ContentBinding(pdf)
	fmt.Fprintf(out, "approval for pdf-processing is bound to %s\n", binding)
	skill, err := skillhost.LoadEntry(ctx, cli, pdf)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "loaded %s: %d bytes of SKILL.md, verified\n", skill.Name(), len(skill.SkillMD()))
	if err := describe(ctx, skill, "templates", out); err != nil {
		return err
	}
	if err := describe(ctx, skill, "templates/regional", out); err != nil {
		return err
	}
	invoice, err := skill.Read(ctx, "templates/invoice.md")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "templates/invoice.md, verified:\n%s", invoice)

	_, err = skill.Read(ctx, "templates/credit-note.md")
	if !skillformat.IsVerificationError(err) {
		return fmt.Errorf("an unlisted file was not refused: %v", err)
	}
	fmt.Fprintf(out, "refused: %v\n", err)

	daily, err := skillhost.Load(ctx, cli, DailyReport)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "loaded %s (dynamic: %v)\n", daily.Name(), daily.Dynamic())
	if err := describe(ctx, daily, ".", out); err != nil {
		return err
	}
	summary, err := daily.Read(ctx, "sections/summary.md")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sections/summary.md, not verifiable:\n%s", summary)
	return nil
}

func findEntry(entries []schema.Skill, uri string) (schema.Skill, error) {
	for _, entry := range entries {
		if entry.Uri == uri {
			return entry, nil
		}
	}
	return schema.Skill{}, fmt.Errorf("the server does not list %s", uri)
}

func describe(ctx context.Context, skill *skillhost.Skill, dir string, out io.Writer) error {
	children, err := skill.List(ctx, dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		name := child.Name
		if child.MimeType != nil && *child.MimeType == schema.DirectoryMimeType {
			name += "/"
		}
		names = append(names, name)
	}
	fmt.Fprintf(out, "%s %s: %s\n", skill.Name(), dir, strings.Join(names, ", "))
	return nil
}

// Host is a client handler for a host that serves no requests of its own
// (no roots, sampling or elicitation), which is all a skills host needs.
type Host struct{}

var _ pclient.Handler = Host{}

func (Host) Notify(context.Context, *jsonrpc.Notification) error   { return nil }
func (Host) NextRequestID() jsonrpc.RequestId                      { return 0 }
func (Host) LastRequestID() jsonrpc.RequestId                      { return 0 }
func (Host) Implements(string) bool                                { return false }
func (Host) Init(context.Context, *schema.ClientCapabilities)      {}
func (Host) OnNotification(context.Context, *jsonrpc.Notification) {}
func (Host) ListRoots(context.Context, *jsonrpc.TypedRequest[*schema.ListRootsRequest]) (*schema.ListRootsResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("roots/list", nil)
}
func (Host) CreateMessage(context.Context, *jsonrpc.TypedRequest[*schema.CreateMessageRequest]) (*schema.CreateMessageResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("sampling/createMessage", nil)
}
func (Host) Elicit(context.Context, *jsonrpc.TypedRequest[*schema.ElicitRequest]) (*schema.ElicitResult, *jsonrpc.Error) {
	return nil, jsonrpc.NewMethodNotFound("elicitation/create", nil)
}
