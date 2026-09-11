package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	log2 "log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
)

// DefaultActorHeader is the HTTP header carrying the calling user's identity
// (for example a Copilot user UPN) when the fronting connector supplies one.
// When absent, audit entries are attributed to DefaultActor.
const DefaultActorHeader = "X-NL-Actor"

// DefaultActor is the audit actor when no actor header is present.
const DefaultActor = "copilot"

// Options configures New. APIToken is required: an empty token refuses
// startup rather than serving an unauthenticated endpoint.
type Options struct {
	// APIToken is the bearer token clients must present. Sourced from the
	// NL_API_KEY environment variable by cmd/workiva-mcp.
	APIToken string
	// ActorHeader overrides DefaultActorHeader.
	ActorHeader string
	// Version is the server implementation version reported to MCP
	// clients. Defaults to "dev".
	Version string
}

// New builds the MCP HTTP handler: an SDK server with every registered
// tool bound, an audit middleware recording each tools/call, and bearer
// auth in front of the /mcp streamable HTTP endpoint.
func New(deps Deps, reg *Registry, opts *Options) (http.Handler, error) {
	if opts == nil {
		opts = &Options{}
	}
	if opts.APIToken == "" {
		return nil, errors.New("mcpserver: APIToken is required; set the NL_API_KEY environment variable")
	}
	actorHeader := opts.ActorHeader
	if actorHeader == "" {
		actorHeader = DefaultActorHeader
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "northern-lights",
		Version: version,
	}, nil)

	if err := reg.Bind(server, deps); err != nil {
		return nil, err
	}

	if deps.Audit != nil {
		server.AddReceivingMiddleware(auditMiddleware(deps.Audit, actorHeader))
	}

	streamableOpts := &mcp.StreamableHTTPOptions{}
	if os.Getenv("NL_DISABLE_LOCALHOST_PROTECTION") == "true" {
		// Public tunnels such as pinggy arrive with a non-localhost Host header.
		// Bearer auth still gates /mcp; this only disables the SDK's DNS rebinding guard.
		streamableOpts.DisableLocalhostProtection = true
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, streamableOpts)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)

	return bearerAuthHandler(opts.APIToken, mux), nil
}

// auditMiddleware appends one audit entry per tools/call request. Tool
// names are recorded as both the tool and the target when no more specific
// target can be derived from the arguments; tools that perform Workiva
// mutations append their own richer entries with before/after values.
// writeTools mutate state and must never execute when the audit log is
// unavailable; an unaudited write is an EU AI Act Art. 12 violation.
var writeTools = map[string]bool{
	"workiva_update_field": true,
	"workiva_sync_mapping": true,
}

func auditMiddleware(log *audit.Log, actorHeader string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				tool, err := recordToolCall(ctx, log, actorHeader, req)
				if err != nil {
					log2.Printf("AUDIT FAILURE: could not record call to %q: %v", tool, err)
					if writeTools[tool] {
						return nil, fmt.Errorf("audit log unavailable, refusing unaudited write via %s: %w", tool, err)
					}
				}
			}
			return next(ctx, method, req)
		}
	}
}

// sanitizeActor trims and validates an actor identity string. The identity
// is asserted by the authenticated MCP client (the bearer token holder) and
// is not independently verified; it is recorded for audit attribution only.
// Overlong or control-character values fall back to DefaultActor.
func sanitizeActor(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 128 {
		return DefaultActor
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return DefaultActor
		}
	}
	return v
}

// ActorFromRequest extracts the sanitized caller identity from the
// X-NL-Actor request header, falling back to DefaultActor.
func ActorFromRequest(req mcp.Request, actorHeader string) string {
	if extra := req.GetExtra(); extra != nil {
		if v := extra.Header.Get(actorHeader); v != "" {
			return sanitizeActor(v)
		}
	}
	return DefaultActor
}

// recordToolCall appends one audit entry per tools/call request and returns
// the tool name plus any append error. Tool names are recorded as both the
// tool and the target when no more specific target can be derived from the
// arguments; tools that perform Workiva mutations append their own richer
// entries with before/after values.
func recordToolCall(ctx context.Context, log *audit.Log, actorHeader string, req mcp.Request) (string, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil {
		return "", nil
	}
	actor := ActorFromRequest(req, actorHeader)
	target := params.Name
	if t := targetFromArguments(params.Arguments); t != "" {
		target = t
	}
	_, err := log.Append(ctx, audit.Entry{
		Actor:  actor,
		Tool:   params.Name,
		Action: "call",
		Target: target,
	})
	return params.Name, err
}

// targetFromArguments extracts a best-effort audit target from raw tool
// arguments, preferring the keys tools conventionally use to name their
// subject. Returns "" when nothing usable is present.
func targetFromArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	for _, key := range []string{"name", "field", "spreadsheet", "range", "target"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// bearerAuthHandler gates every request behind a bearer token compared in
// constant time. The 401 body is plain text so accidental hits from browsers
// are self-explanatory.
func bearerAuthHandler(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) ||
			subtle.ConstantTimeCompare([]byte(auth[len(prefix):]), expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="northern-lights"`)
			http.Error(w, "unauthorized: missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// EnvAPIToken reads the bearer token from the environment, refusing an
// empty value so a misconfigured deployment fails loudly at startup.
func EnvAPIToken() (string, error) {
	token := strings.TrimSpace(os.Getenv("NL_API_KEY"))
	if token == "" {
		return "", fmt.Errorf("mcpserver: NL_API_KEY is not set")
	}
	return token, nil
}
