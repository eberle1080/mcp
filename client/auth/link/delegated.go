package link

import "github.com/eberle1080/mcp/client/auth/link/providerregistry"

type DelegatedMCPAuth struct {
	registry *providerregistry.Registry
	resolver *DelegatedCredentialResolver
}
