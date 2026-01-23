package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eberle1080/jsonrpc/transport/server/base"
	streamauth "github.com/eberle1080/jsonrpc/transport/server/auth"
	"github.com/eberle1080/jsonrpc/transport/server/http/sse"
	"github.com/eberle1080/jsonrpc/transport/server/http/streamable"
	mcpauth "github.com/eberle1080/mcp/server/auth"
)

type httpServer struct {
	sseHandler         *sse.Handler
	streamingHandler   *streamable.Handler
	useStreamableHTTP  bool
	addr               string
	customHTTPHandlers map[string]http.HandlerFunc
	sseURI             string
	sseMessageURI      string
	streamableURI      string
	rootRedirect       bool
	sessionStore       base.SessionStore
}

// UseStreamableHTTP sets whether to use streamableHTTP or SSE for the HTTP handler.
func (s *Server) UseStreamableHTTP(flag bool) {
	s.useStreamableHTTP = flag
}

// HTTP creates and returns an HTTP handler with OAuth2 authorizer and SSE handlers.
func (s *Server) HTTP(_ context.Context, addr string) *http.Server {
	if addr == "" {
		addr = s.addr
	}
	if addr == "" {
		// Default bind only to localhost to reduce DNS rebinding risk
		addr = "127.0.0.1:5000"
	}
	// Defaults if not provided via options
	if s.sseURI == "" {
		s.sseURI = "/sse"
	}
	if s.sseMessageURI == "" {
		s.sseMessageURI = "/message"
	}
	if s.streamableURI == "" {
		s.streamableURI = "/mcp"
	}

	// Default in-memory BFF AuthStore (dev-friendly). Replace via server options for production.
	memAuth := streamauth.NewMemoryStore(30*time.Minute, 24*time.Hour, 2*time.Minute)
	mcpauth.SetDefaultBFFAuthStore(memAuth)
	mcpauth.SetDefaultBFFAuthCookieName("BFF-Auth-Session")
	if serverDebug() {
		fmt.Printf("[mcp/server] HTTP addr=%s sseURI=%s msgURI=%s streamURI=%s\n", addr, s.sseURI, s.sseMessageURI, s.streamableURI)
	}
	if serverDebug() {
		fmt.Printf("[mcp/server] BFF auth: memStore=yes authCookie=BFF-Auth-Session rehydrate=true\n")
	}

	// SSE and Streamable handlers with configured URIs
	// Enable BFF auth cookie (opaque grant) and handshake rehydrate; do NOT set transport session in cookies.
	sseOptions := []sse.Option{
		sse.WithURI(s.sseURI),
		sse.WithMessageURI(s.sseMessageURI),
		// Enable auth cookie and rehydrate from it
		sse.WithAuthStore(memAuth),
		sse.WithBFFAuthCookie(&sse.BFFAuthCookie{Name: "BFF-Auth-Session", HttpOnly: true}),
		sse.WithRehydrateOnHandshake(true),
	}
	streamableOptions := []streamable.Option{
		streamable.WithURI(s.streamableURI),
		// Enable auth cookie and rehydrate from it
		streamable.WithAuthStore(memAuth),
		streamable.WithBFFAuthCookie(&streamable.BFFAuthCookie{Name: "BFF-Auth-Session", HttpOnly: true}),
		streamable.WithRehydrateOnHandshake(true),
	}

	// If a custom session store is provided, use it for both handlers
	if s.sessionStore != nil {
		sseOptions = append(sseOptions, sse.WithSessionStore(s.sessionStore))
		streamableOptions = append(streamableOptions, streamable.WithSessionStore(s.sessionStore))
	}

	s.sseHandler = sse.New(s.NewHandler, sseOptions...)
	s.streamingHandler = streamable.New(s.NewHandler, streamableOptions...)
	mux := http.NewServeMux()
	if len(s.customHTTPHandlers) > 0 {
		for path, handler := range s.customHTTPHandlers {
			mux.Handle(path, handler)
		}
	}
	if s.protectedResourcesHandler != nil {
		mux.Handle("/.well-known/oauth-protected-resource", s.protectedResourcesHandler)
	}
	var middlewareHandlers []Middleware
	if s.authorizer != nil {
		middlewareHandlers = append(middlewareHandlers, s.authorizer)
	}
	// Validate MCP-Protocol-Version and set response header
	middlewareHandlers = append(middlewareHandlers, protocolVersionMiddleware())
	middlewareHandlers = append(middlewareHandlers, s.corsHandler)
	// Validate Origin on all requests (uses configured CORS allowlist)
	if s.corsConfig != nil {
		middlewareHandlers = append(middlewareHandlers, originValidationMiddleware(s.corsConfig.AllowOrigins))
	}
	// Wrap handlers with middleware
	sseChain := ChainMiddlewareHandlers(s.sseHandler, middlewareHandlers...)
	streamChain := ChainMiddlewareHandlers(s.streamingHandler, middlewareHandlers...)

	// Mount handlers at their base URIs
	mux.Handle(s.sseURI, sseChain)
	mux.Handle(s.sseMessageURI, sseChain)
	mux.Handle(s.streamableURI, streamChain)

	// Optional root redirect to the active transport base
	if s.rootRedirect {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			target := s.sseURI
			if s.useStreamableHTTP {
				target = s.streamableURI
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		})
	}
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return server
}

var srvDbg struct {
	once sync.Once
	v    bool
}

func serverDebug() bool {
	srvDbg.once.Do(func() {
		v := strings.ToLower(strings.TrimSpace(os.Getenv("MCP_DEBUG")))
		srvDbg.v = v != "" && v != "0" && v != "false"
	})
	return srvDbg.v
}
