// Package skillhost loads skills that an MCP server serves (the
// io.modelcontextprotocol/skills extension, SEP-2640) and enforces what the
// extension requires of a host when it reads them:
//
//   - nothing is fetched ahead of need: loading reads SKILL.md, and a
//     supporting file is read when asked for;
//   - every read goes to the server that serves the skill, and only for a
//     file inside the skill;
//   - for a skill whose entry lists its files, only a listed file is read,
//     and its bytes are used only when their size and SHA-256 digest match
//     the entry;
//   - SKILL.md's frontmatter must equal the entry's, field by field.
//
// A failed check returns a *skills.VerificationError (from
// github.com/eberle1080/mcp-protocol/extension/skills). The content is not
// returned. The recovery is to fetch the entry again with skills/get and, if
// its files changed, to ask the user again.
//
// This package does not decide whether a skill may be loaded. Asking the
// user, binding that approval to skills.ContentBinding, showing the skill's
// origin to the model and the user, ignoring allowed-tools, and keeping
// same-named skills from different servers apart remain the application's
// job. docs/skills.md lists them.
package skillhost

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"

	skillformat "github.com/eberle1080/mcp-protocol/extension/skills"
	"github.com/eberle1080/mcp-protocol/schema"
	"github.com/eberle1080/mcp/client"
)

// SEP-2640's per-skill limits. A host must accept skills up to them.
const (
	MaxSkillFiles = 512
	MaxSkillBytes = 16 << 20
)

// maxDirectoryPages bounds how many resources/directory/read pages one List
// follows, so a server cannot keep a host paging forever.
const maxDirectoryPages = 64

// Source is one server connection. *client.Client, *server.Adapter and the
// client pool's managed clients satisfy it.
type Source interface {
	GetSkill(ctx context.Context, uri string, options ...client.RequestOption) (*schema.GetSkillResult, error)
	ReadResource(ctx context.Context, params *schema.ReadResourceRequestParams, options ...client.RequestOption) (*schema.ReadResourceResult, error)
}

// DirectorySource is a Source that can call resources/directory/read. List
// uses it for skills whose files are not listed in their entry.
type DirectorySource interface {
	ReadResourceDirectory(ctx context.Context, uri string, cursor *string, options ...client.RequestOption) (*schema.ReadResourceDirectoryResult, error)
}

// Skill is a loaded skill: the entry it was loaded from (the held entry) and
// its verified SKILL.md. Reads through it are checked against that entry for
// as long as the application holds it. It is safe for concurrent use.
type Skill struct {
	source  Source
	options []client.RequestOption
	entry   schema.Skill
	root    string
	skillMD []byte

	mu        sync.Mutex
	verified  map[string][]byte
	retrieved int64
}

// Load fetches the skill's entry with skills/get and loads the skill from it.
func Load(ctx context.Context, source Source, uri string, options ...client.RequestOption) (*Skill, error) {
	result, err := source.GetSkill(ctx, uri, options...)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("skills/get returned no entry for %s", uri)
	}
	if result.Skill.Uri != uri {
		return nil, fmt.Errorf("skills/get for %s returned the entry for %s", uri, result.Skill.Uri)
	}
	return LoadEntry(ctx, source, result.Skill, options...)
}

// LoadEntry loads a skill from an entry the application already holds, such
// as one from skills/list. It checks the entry against the extension's rules
// and limits, reads SKILL.md, and verifies it. It reads nothing else.
func LoadEntry(ctx context.Context, source Source, entry schema.Skill, options ...client.RequestOption) (*Skill, error) {
	if source == nil {
		return nil, fmt.Errorf("skill source is required")
	}
	held, err := skillformat.Clone(entry)
	if err != nil {
		return nil, err
	}
	if err := skillformat.Validate(held); err != nil {
		return nil, fmt.Errorf("skill %s cannot be loaded: %w", entry.Uri, err)
	}
	root, err := skillformat.RootOf(held)
	if err != nil {
		return nil, err
	}
	skill := &Skill{
		source:   source,
		options:  append([]client.RequestOption(nil), options...),
		entry:    held,
		root:     root,
		verified: map[string][]byte{},
	}
	data, err := skill.fetch(ctx, held.Uri)
	if err != nil {
		return nil, err
	}
	if err := skillformat.VerifyFrontmatter(held, data); err != nil {
		return nil, err
	}
	skill.skillMD = data
	return skill, nil
}

// Entry returns a copy of the held entry.
func (s *Skill) Entry() schema.Skill {
	entry, _ := skillformat.Clone(s.entry)
	return entry
}

// URI is the URI of the skill's SKILL.md.
func (s *Skill) URI() string { return s.entry.Uri }

// Root is the URI of the skill's root directory.
func (s *Skill) Root() string { return s.root }

// Name is the skill's frontmatter name. Names are labels, not identifiers:
// two servers, or two paths on one server, can serve skills with the same name.
func (s *Skill) Name() string {
	name, _ := s.entry.Frontmatter["name"].(string)
	return name
}

// Dynamic reports whether the server publishes no digests for the skill. Its
// supporting files cannot be verified, and an approval cannot be bound to them.
func (s *Skill) Dynamic() bool { return s.entry.Resources.Dynamic }

// SkillMD returns the verified SKILL.md.
func (s *Skill) SkillMD() []byte { return bytes.Clone(s.skillMD) }

// Read returns one of the skill's files, named by a path relative to the skill
// root ("references/GUIDE.md"), as SKILL.md would refer to it, or by its URI.
// For a skill whose entry lists its files, a file the entry does not list is a
// verification failure, and so are bytes that do not match the entry. Verified
// files are kept, so reading one again does not go back to the server.
func (s *Skill) Read(ctx context.Context, ref string) ([]byte, error) {
	uri, err := s.resolve(ref)
	if err != nil {
		return nil, err
	}
	if uri == s.root {
		return nil, fmt.Errorf("%s is not a file of skill %s", uri, s.entry.Uri)
	}
	if uri == s.entry.Uri {
		return s.SkillMD(), nil
	}
	return s.fetch(ctx, uri)
}

// resolve turns a reference into the URI of something inside the skill. The
// URI must be spelled canonically and sit under the skill root, so that
// nothing (a dot segment, an escaped slash, a query) can make a read of it
// reach outside the skill on a server that normalizes URIs.
func (s *Skill) resolve(ref string) (string, error) {
	uri, err := skillformat.Resolve(s.entry, ref)
	if err != nil {
		return "", err
	}
	if !skillformat.Within(s.root, uri) {
		return "", fmt.Errorf("%s is not inside skill %s", uri, s.entry.Uri)
	}
	if err := skillformat.Canonical(uri); err != nil {
		return "", fmt.Errorf("skill %s: %w", s.entry.Uri, err)
	}
	return uri, nil
}

// fetch reads uri from the skill's own server and checks it.
func (s *Skill) fetch(ctx context.Context, uri string) ([]byte, error) {
	var file schema.SkillResource
	if !s.entry.Resources.Dynamic {
		s.mu.Lock()
		data, ok := s.verified[uri]
		s.mu.Unlock()
		if ok {
			return bytes.Clone(data), nil
		}
		listed, ok := skillformat.File(s.entry, uri)
		if !ok {
			return nil, &skillformat.VerificationError{URI: uri, Reason: "the held entry does not list this file"}
		}
		file = listed
	}
	result, err := s.source.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: uri}, s.options...)
	if err != nil {
		return nil, err
	}
	data, err := skillformat.ContentBytes(uri, result)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry.Resources.Dynamic {
		// Nothing to verify against; bound what a dynamic skill can deliver.
		if s.retrieved+int64(len(data)) > MaxSkillBytes {
			return nil, fmt.Errorf("skill %s has delivered more than %d bytes", s.entry.Uri, MaxSkillBytes)
		}
		s.retrieved += int64(len(data))
		return data, nil
	}
	if err := skillformat.VerifyFile(file, data); err != nil {
		return nil, err
	}
	s.verified[uri] = bytes.Clone(data)
	return data, nil
}

// List returns the direct children of a directory in the skill, named like
// Read names files ("templates", or "." for the root). Subdirectories carry
// the mimeType "inode/directory". For a skill whose entry lists its files, the
// answer comes from the entry alone, so it never shows a file the entry does
// not list. For a dynamic skill it asks the server with
// resources/directory/read, which works only if the server declared it.
func (s *Skill) List(ctx context.Context, dir string) ([]schema.Resource, error) {
	uri, err := s.resolve(dir)
	if err != nil {
		return nil, err
	}
	if !s.entry.Resources.Dynamic {
		return s.listFromEntry(uri)
	}
	reader, ok := s.source.(DirectorySource)
	if !ok {
		return nil, fmt.Errorf("the connection to the server of skill %s cannot read directories", s.entry.Uri)
	}
	var children []schema.Resource
	var cursor *string
	for page := 0; ; page++ {
		if page == maxDirectoryPages {
			return nil, fmt.Errorf("directory %s has more than %d pages", uri, maxDirectoryPages)
		}
		result, err := reader.ReadResourceDirectory(ctx, uri, cursor, s.options...)
		if err != nil {
			return nil, err
		}
		for _, child := range result.Resources {
			if child.Uri != uri && strings.HasPrefix(child.Uri, uri+"/") && !strings.Contains(strings.TrimPrefix(child.Uri, uri+"/"), "/") {
				children = append(children, child)
			}
		}
		if result.NextCursor == nil || *result.NextCursor == "" {
			return children, nil
		}
		cursor = result.NextCursor
	}
}

func (s *Skill) listFromEntry(uri string) ([]schema.Resource, error) {
	prefix := uri + "/"
	children := map[string]schema.Resource{}
	for _, file := range s.entry.Resources.Files {
		rest, ok := strings.CutPrefix(file.Uri, prefix)
		if !ok || rest == "" {
			continue
		}
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			child := prefix + rest[:slash]
			if _, seen := children[child]; !seen {
				mimeType := schema.DirectoryMimeType
				children[child] = schema.Resource{Uri: child, Name: unescape(rest[:slash]), MimeType: &mimeType}
			}
			continue
		}
		size := int(file.Size)
		children[file.Uri] = schema.Resource{Uri: file.Uri, Name: unescape(path.Base(rest)), Size: &size}
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("skill %s has no directory %s", s.entry.Uri, uri)
	}
	result := make([]schema.Resource, 0, len(children))
	for _, child := range children {
		result = append(result, child)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Uri < result[j].Uri })
	return result, nil
}

func unescape(segment string) string {
	if decoded, err := url.PathUnescape(segment); err == nil {
		return decoded
	}
	return segment
}
