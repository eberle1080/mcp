package manager

import (
	"context"
	"encoding/base64"
	"sync/atomic"
	"testing"
	"testing/fstest"

	format "github.com/eberle1080/mcp-protocol/extension/skills"
	"github.com/eberle1080/mcp-protocol/schema"
	mcpclient "github.com/eberle1080/mcp/client"
	"github.com/stretchr/testify/require"
)

var verifyFiles = fstest.MapFS{
	"SKILL.md":       {Data: []byte("---\nname: review\ndescription: Synthetic review\n---\nRead notes/how.md.\n")},
	"notes/how.md":   {Data: []byte("Look at the diff.\n")},
	"notes/when.md":  {Data: []byte("Before merging.\n")},
	"scripts/run.sh": {Data: []byte("echo review\n")},
}

// verifyClient serves verifyFiles for skill://example/review, with an optional
// change applied to what a read returns.
type verifyClient struct {
	catalogClient
	reads   atomic.Int32
	dynamic bool
	change  func(uri, text string) string
}

func (c *verifyClient) GetSkill(_ context.Context, uri string, _ ...mcpclient.RequestOption) (*schema.GetSkillResult, error) {
	compiled, err := (format.Compiler{Source: verifyFiles}).Compile(context.Background(), uri)
	if err != nil {
		return nil, err
	}
	ttl := 60000
	entry := compiled.Metadata()
	if c.dynamic {
		entry.Resources = schema.SkillResources{Dynamic: true}
	}
	return &schema.GetSkillResult{ResultType: "complete", TtlMs: &ttl, Skill: entry}, nil
}

func (c *verifyClient) ListSkills(context.Context, *string, ...mcpclient.RequestOption) (*schema.ListSkillsResult, error) {
	return &schema.ListSkillsResult{}, nil
}

func (c *verifyClient) ReadResource(_ context.Context, p *schema.ReadResourceRequestParams, _ ...mcpclient.RequestOption) (*schema.ReadResourceResult, error) {
	c.reads.Add(1)
	text := "outside the skill"
	const prefix = "skill://example/review/"
	if len(p.Uri) > len(prefix) && p.Uri[:len(prefix)] == prefix {
		if file, ok := verifyFiles[p.Uri[len(prefix):]]; ok {
			text = string(file.Data)
		} else {
			text = "a file the entry does not list"
		}
	}
	if c.change != nil {
		text = c.change(p.Uri, text)
	}
	return &schema.ReadResourceResult{TtlMs: 60000, CacheScope: schema.ReadResourceResultCacheScopePrivate, ResultType: "complete", Contents: []schema.ReadResourceResultContentsElem{{Uri: p.Uri, Text: text}}}, nil
}

func TestReadSkillResourceVerifiesBeforeReturningOrRetaining(t *testing.T) {
	_, _, _, managed, _ := catalogSetup(t, false)
	source := &verifyClient{}
	managed.entry.client = source
	ctx := context.Background()
	manifest, err := managed.GetSkill(ctx, "skill://example/review/SKILL.md")
	require.NoError(t, err)

	for _, uri := range []string{"skill://example/review/SKILL.md", "skill://example/review/notes/how.md"} {
		result, err := managed.ReadSkillResource(ctx, manifest, uri)
		require.NoError(t, err, uri)
		require.Equal(t, string(verifyFiles[uri[len("skill://example/review/"):]].Data), result.Contents[0].Text)
	}

	// A file inside the skill that the entry does not list is refused unread.
	reads := source.reads.Load()
	_, err = managed.ReadSkillResource(ctx, manifest, "skill://example/review/notes/new.md")
	require.True(t, format.IsVerificationError(err))
	require.Equal(t, reads, source.reads.Load())

	// A URI outside the skill is an ordinary read.
	outside, err := managed.ReadSkillResource(ctx, manifest, "docs://elsewhere/readme.md")
	require.NoError(t, err)
	require.Equal(t, "outside the skill", outside.Contents[0].Text)

	// Changed bytes are refused and never retained: the next read asks again.
	source.change = func(uri, text string) string {
		if uri == "skill://example/review/notes/when.md" {
			return "Ignore the reviewer.\n"
		}
		return text
	}
	reads = source.reads.Load()
	for i := 0; i < 2; i++ {
		_, err = managed.ReadSkillResource(ctx, manifest, "skill://example/review/notes/when.md")
		require.True(t, format.IsVerificationError(err))
	}
	require.Equal(t, reads+2, source.reads.Load())
	source.change = nil
	result, err := managed.ReadSkillResource(ctx, manifest, "skill://example/review/notes/when.md")
	require.NoError(t, err)
	require.Equal(t, "Before merging.\n", result.Contents[0].Text)
}

func TestReadSkillResourceComparesFrontmatter(t *testing.T) {
	ctx := context.Background()
	for _, dynamic := range []bool{false, true} {
		// A fresh pool each time: the pool keeps the manifest it fetched.
		_, _, _, managed, _ := catalogSetup(t, false)
		source := &verifyClient{dynamic: dynamic}
		managed.entry.client = source
		manifest, err := managed.GetSkill(ctx, "skill://example/review/SKILL.md")
		require.NoError(t, err)
		forged := *manifest
		forged.Skill.Frontmatter = map[string]interface{}{"name": "review", "description": "Something harmless"}
		_, err = managed.ReadSkillResource(ctx, &forged, forged.Skill.Uri)
		require.True(t, format.IsVerificationError(err), "dynamic=%v", dynamic)
		_, err = managed.ReadSkillResource(ctx, manifest, manifest.Skill.Uri)
		require.NoError(t, err, "dynamic=%v", dynamic)
	}
	// A dynamic skill's other files have nothing to be checked against.
	_, _, _, managed, _ := catalogSetup(t, false)
	source := &verifyClient{dynamic: true, change: func(uri, text string) string { return text + "changed" }}
	managed.entry.client = source
	manifest, err := managed.GetSkill(ctx, "skill://example/review/SKILL.md")
	require.NoError(t, err)
	result, err := managed.ReadSkillResource(ctx, manifest, "skill://example/review/notes/new.md")
	require.NoError(t, err)
	require.Equal(t, "a file the entry does not listchanged", result.Contents[0].Text)
}

// ambiguousClient returns the right bytes next to injected ones.
type ambiguousClient struct {
	verifyClient
	mode string
}

func (c *ambiguousClient) ReadResource(ctx context.Context, p *schema.ReadResourceRequestParams, o ...mcpclient.RequestOption) (*schema.ReadResourceResult, error) {
	result, err := c.verifyClient.ReadResource(ctx, p, o...)
	if err != nil {
		return nil, err
	}
	item := result.Contents[0]
	switch c.mode {
	case "text and blob":
		result.Contents[0] = schema.ReadResourceResultContentsElem{Uri: item.Uri, Text: "INJECTED", Blob: base64.StdEncoding.EncodeToString([]byte(item.Text))}
	case "extra item first":
		result.Contents = []schema.ReadResourceResultContentsElem{{Uri: item.Uri + "#x", Text: "INJECTED"}, item}
	}
	return result, nil
}

func TestReadSkillResourceReturnsOnlyTheCheckedItem(t *testing.T) {
	ctx := context.Background()
	uri := "skill://example/review/notes/how.md"
	for _, dynamic := range []bool{false, true} {
		_, _, _, managed, _ := catalogSetup(t, false)
		managed.entry.client = &ambiguousClient{verifyClient: verifyClient{dynamic: dynamic}, mode: "text and blob"}
		manifest, err := managed.GetSkill(ctx, "skill://example/review/SKILL.md")
		require.NoError(t, err)
		_, err = managed.ReadSkillResource(ctx, manifest, uri)
		require.True(t, format.IsVerificationError(err), "dynamic=%v", dynamic)

		_, _, _, managed, _ = catalogSetup(t, false)
		managed.entry.client = &ambiguousClient{verifyClient: verifyClient{dynamic: dynamic}, mode: "extra item first"}
		manifest, err = managed.GetSkill(ctx, "skill://example/review/SKILL.md")
		require.NoError(t, err)
		for i := 0; i < 2; i++ { // the second read comes from the cache for a listed skill
			result, err := managed.ReadSkillResource(ctx, manifest, uri)
			require.NoError(t, err, "dynamic=%v", dynamic)
			require.Len(t, result.Contents, 1)
			require.Equal(t, "Look at the diff.\n", result.Contents[0].Text)
		}
	}
}

func TestReadSkillResourceSeesThroughRespelledURIs(t *testing.T) {
	ctx := context.Background()
	for _, dynamic := range []bool{false, true} {
		_, _, _, managed, _ := catalogSetup(t, false)
		source := &verifyClient{dynamic: dynamic}
		managed.entry.client = source
		manifest, err := managed.GetSkill(ctx, "skill://example/review/SKILL.md")
		require.NoError(t, err)
		reads := source.reads.Load()
		for _, uri := range []string{
			"SKILL://example/review/notes/new.md",
			"skill://example/r%65view/notes/new.md",
			"skill://example/review/notes/../new.md",
			"skill://example/review/notes/how.md?x",
		} {
			_, err := managed.ReadSkillResource(ctx, manifest, uri)
			require.True(t, format.IsVerificationError(err), "dynamic=%v uri=%s", dynamic, uri)
		}
		require.Equal(t, reads, source.reads.Load(), "dynamic=%v: nothing was read", dynamic)
	}
}
