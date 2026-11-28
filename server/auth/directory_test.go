package auth

import (
	"context"
	"fmt"
	"testing"

	"github.com/eberle1080/jsonrpc"
	"github.com/eberle1080/mcp-protocol/authorization"
	"github.com/eberle1080/mcp-protocol/schema"
)

// A directory listing names the files under it, so it needs every rule that
// protects one of them, whether or not plain resource reads are enforced.
func TestDirectoryReadNeedsTheRulesOfEveryFileBelowIt(t *testing.T) {
	policy := &authorization.Policy{Resources: map[string]*authorization.Authorization{
		"skill://private/SKILL.md":            {RequiredScopes: []string{"manifest:read"}},
		"skill://private/references/guide.md": {RequiredScopes: []string{"reference:read"}},
	}}
	grants := map[string][]string{
		"verified":       {"manifest:read", "reference:read"},
		"manifest-only":  {"manifest:read"},
		"reference-only": {"reference:read"},
	}
	for _, tc := range []struct {
		directory string
		allowed   []string
	}{
		{"skill://private", []string{"verified"}},
		{"skill://private/references", []string{"verified", "reference-only"}},
		{"skill://public", []string{"", "verified", "manifest-only", "reference-only"}},
		// A sibling whose name extends the protected one is not under it.
		{"skill://private/references-old", []string{"", "verified", "manifest-only", "reference-only"}},
	} {
		for _, enforced := range []bool{false, true} {
			for _, credential := range []string{"", "verified", "manifest-only", "reference-only"} {
				service, err := New(&Config{Policy: policy, RequireResourceAuthorization: enforced, AuthorizeResource: func(_ context.Context, token *authorization.Token, rule *authorization.Authorization) error {
					if token != nil {
						for _, scope := range grants[token.Token] {
							if scope == rule.RequiredScopes[0] {
								return nil
							}
						}
					}
					return fmt.Errorf("denied")
				}})
				if err != nil {
					t.Fatal(err)
				}
				request, err := jsonrpc.NewRequest(schema.MethodResourcesDirectoryRead, &schema.ReadResourceDirectoryRequestParams{Uri: tc.directory})
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if credential != "" {
					ctx = context.WithValue(ctx, authorization.TokenKey, &authorization.Token{Token: credential})
				}
				response := &jsonrpc.Response{}
				if _, err = service.EnsureAuthorized(ctx, request, response); err != nil {
					t.Fatal(err)
				}
				want := false
				for _, allowed := range tc.allowed {
					want = want || allowed == credential
				}
				if (response.Error == nil) != want {
					t.Fatalf("directory=%s enforced=%v credential=%q err=%v", tc.directory, enforced, credential, response.Error)
				}
			}
		}
	}
}

func TestDirectoryReadMiddlewareRuleCoversFilesBelow(t *testing.T) {
	policy := &authorization.Policy{Resources: map[string]*authorization.Authorization{
		"skill://private/references/guide.md": {RequiredScopes: []string{"reference:read"}},
	}}
	service, err := New(&Config{Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	for directory, protected := range map[string]bool{"skill://private": true, "skill://private/references": true, "skill://public": false} {
		request, err := jsonrpc.NewRequest(schema.MethodResourcesDirectoryRead, &schema.ReadResourceDirectoryRequestParams{Uri: directory})
		if err != nil {
			t.Fatal(err)
		}
		rule, _ := service.resolveAuthorizationRule(request)
		if (rule != nil) != protected {
			t.Fatalf("directory=%s rule=%+v", directory, rule)
		}
	}
}
