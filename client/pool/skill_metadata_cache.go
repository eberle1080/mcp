package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	skillformat "github.com/eberle1080/mcp-protocol/extension/skills"
	"github.com/eberle1080/mcp-protocol/schema"
	mcpclient "github.com/eberle1080/mcp/client"
	authctx "github.com/eberle1080/mcp/client/auth/link"
)

var errSkillNotRetained = errors.New("skill metadata is not retained")

// skillMetadata uses the existing scoped, bounded, singleflight metadata store.
// Explicit non-cacheable responses are delivered without retaining their bytes.
func (c *managedClient) skillMetadata(ctx context.Context, entryKey string, load func(context.Context) (json.RawMessage, *int, error), opts ...mcpclient.RequestOption) (json.RawMessage, error) {
	scoped, config, scope, witness, token, cacheable, err := c.toolCatalogScope(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if !cacheable {
		v, _, err := load(scoped)
		return v, err
	}
	// Skill metadata is always private, even when tools/list is public.
	private, _ := json.Marshal([]string{c.key, scope, authctx.EffectiveUserID(scoped), authctx.CanonicalUserID(scoped), authctx.Provider(scoped), authctx.Bearer(scoped), authctx.IDToken(scoped)})
	c.mgr.mu.Lock()
	generation := c.mgr.toolCatalogGeneration[c.serverName]
	c.mgr.mu.Unlock()
	key := fmt.Sprintf("skill:%p:%s:%x:%x:%d", c.entry, c.serverName, sha256.Sum256(private), sha256.Sum256(config), generation)
	partition := c.mgr.WithCatalogSources(scoped, c.mgr.CatalogPartition(scoped, scope, "skill"), []string{c.serverName})
	var unretained json.RawMessage
	raw, err := c.mgr.VisibleCatalogEntryInPartition(scoped, key, partition, entryKey, func(ctx context.Context) (json.RawMessage, time.Time, error) {
		value, ttl, err := load(ctx)
		if err != nil {
			return nil, time.Time{}, err
		}
		_, freshConfig, freshScope, _, _, freshCacheable, scopeErr := c.toolCatalogScope(ctx, opts...)
		valid := scopeErr == nil && freshCacheable && freshScope == scope && bytes.Equal(config, freshConfig)
		if witness != nil {
			witness.mu.Lock()
			valid = valid && witness.observed && witness.token == token
			witness.mu.Unlock()
		}
		if !valid {
			return nil, time.Time{}, fmt.Errorf("skill metadata credential changed during read")
		}
		if ttl == nil || *ttl <= 0 {
			unretained = value
			return nil, time.Time{}, errSkillNotRetained
		}
		duration := 5 * time.Minute
		if *ttl < int(duration/time.Millisecond) {
			duration = time.Duration(*ttl) * time.Millisecond
		}
		return value, time.Now().Add(duration), nil
	})
	if errors.Is(err, errSkillNotRetained) {
		return unretained, nil
	}
	return raw, err
}

// ReadSkillResource reads a file of the skill the manifest describes and checks
// it the way SEP-2640 requires of a host. A file the manifest lists is
// returned only when its size and digest match (and, for SKILL.md, its
// frontmatter); verified results are retained for the manifest's lifetime. A
// file inside the skill that the manifest does not list is a verification
// failure, whatever its spelling. For a dynamic skill only SKILL.md's
// frontmatter can be checked, and nothing is retained. The result holds only
// the content item that was checked. A URI outside the skill is an ordinary,
// uncached read. Failures are *skills.VerificationError values; the recovery
// is a fresh GetSkill.
func (c *managedClient) ReadSkillResource(ctx context.Context, manifest *schema.GetSkillResult, uri string, opts ...mcpclient.RequestOption) (*schema.ReadResourceResult, error) {
	_, release, err := c.use()
	if err != nil {
		return nil, err
	}
	defer release()
	if manifest == nil || manifest.ResultType != "complete" {
		return c.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: uri}, opts...)
	}
	root, err := skillformat.RootOf(manifest.Skill)
	if err != nil || !skillformat.Inside(root, uri) {
		return c.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: uri}, opts...)
	}
	if manifest.Skill.Resources.Dynamic {
		if err := skillformat.Canonical(uri); err != nil {
			return nil, &skillformat.VerificationError{URI: uri, Reason: err.Error()}
		}
		result, err := c.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: uri}, opts...)
		if err != nil {
			return nil, err
		}
		item, data, err := skillformat.ContentItem(uri, result)
		if err != nil {
			return nil, err
		}
		if uri == manifest.Skill.Uri {
			if err := skillformat.VerifyFrontmatter(manifest.Skill, data); err != nil {
				return nil, err
			}
		}
		return onlyItem(result, item), nil
	}
	file, declared := skillformat.File(manifest.Skill, uri)
	if !declared {
		return nil, &skillformat.VerificationError{URI: uri, Reason: "the skill's entry does not list this file"}
	}
	revision, err := json.Marshal(manifest.Skill)
	if err != nil {
		return nil, err
	}
	raw, err := c.skillMetadata(ctx, fmt.Sprintf("%s\x00document:%x", uri, sha256.Sum256(revision)), func(ctx context.Context) (json.RawMessage, *int, error) {
		result, err := c.ReadResource(ctx, &schema.ReadResourceRequestParams{Uri: uri}, opts...)
		if err != nil {
			return nil, nil, err
		}
		// Verify before retaining, and retain only the checked item, so only
		// checked bytes are ever returned or reused.
		item, data, err := skillformat.ContentItem(uri, result)
		if err != nil {
			return nil, nil, err
		}
		if err := skillformat.VerifyFile(file, data); err != nil {
			return nil, nil, err
		}
		if uri == manifest.Skill.Uri {
			if err := skillformat.VerifyFrontmatter(manifest.Skill, data); err != nil {
				return nil, nil, err
			}
		}
		result = onlyItem(result, item)
		raw, err := json.Marshal(result)
		// A manifest describes immutable bytes; the resource response controls
		// how long the caller may reuse the read. Honor the shorter lifetime.
		ttl := result.TtlMs
		if manifest.TtlMs == nil || result.ResultType != "" && result.ResultType != "complete" {
			ttl = 0
		} else if *manifest.TtlMs < ttl {
			ttl = *manifest.TtlMs
		}
		return raw, &ttl, err
	}, opts...)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	// In-process and older clients may omit cacheScope. The cache must preserve
	// that already-decoded value, not impose newer wire enum validation.
	type resourceValue schema.ReadResourceResult
	value := new(resourceValue)
	decoded := struct {
		*resourceValue
		CacheScope string `json:"cacheScope"`
	}{resourceValue: value}
	err = json.Unmarshal(raw, &decoded)
	if err != nil {
		return nil, err
	}
	value.CacheScope = schema.ReadResourceResultCacheScope(decoded.CacheScope)
	return (*schema.ReadResourceResult)(value), nil
}

// onlyItem returns a copy of result whose contents are just the checked item.
func onlyItem(result *schema.ReadResourceResult, item schema.ReadResourceResultContentsElem) *schema.ReadResourceResult {
	copy := *result
	copy.Contents = []schema.ReadResourceResultContentsElem{item}
	return &copy
}
