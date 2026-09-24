package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	log2 "log"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/identity"
)

// ActorHeader is the primary HTTP header carrying the calling user's
// identity (for example a Copilot user UPN) when the fronting connector
// supplies one. It is lowercase and has no "X-" prefix because Copilot
// Studio MCP connectors cannot send "X-" headers.
//
// TESTING ONLY. The value is asserted by whoever holds the API key and is
// not verified by Northern Lights. For customer deployments the actor must
// come from the OAuth token, never from a request header.
const ActorHeader = "nl-actor"

// DefaultActorHeader is the fallback actor header, consulted only when
// ActorHeader is absent. Options.ActorHeader overrides the fallback name.
const DefaultActorHeader = "X-NL-Actor"

// DefaultActor is recorded on read operations when no valid actor header is
// present. Write operations are refused instead (see auditMiddleware).
const DefaultActor = "unknown"

// maxActorLen bounds the recorded actor identity.
const maxActorLen = 256

// Options configures New. API-key mode requires APIToken; Entra mode requires
// TokenVerifier. Missing authentication configuration always refuses startup.
type Options struct {
	// AuthMode defaults to api_key for Phase 1 compatibility.
	AuthMode config.AuthMode
	// APIToken is the bearer token clients must present. Sourced from the
	// NL_API_KEY environment variable by cmd/workiva-mcp in api_key mode.
	APIToken string
	// TokenVerifier is required in entra mode and validates access tokens.
	TokenVerifier identity.TokenVerifier
	// ActorHeader overrides DefaultActorHeader.
	ActorHeader string
	// Version is the server implementation version reported to MCP
	// clients. Defaults to "dev".
	Version string
	// DisableLocalhostProtection permits non-localhost Host headers. It is
	// intended only when a trusted HTTPS ingress cannot preserve localhost.
	DisableLocalhostProtection bool
}

// New builds the MCP HTTP handler: an SDK server with every registered tool
// bound, audit and authorization middleware around tools/call, and the selected
// bearer authentication mode in front of the /mcp streamable HTTP endpoint.
func New(deps Deps, reg *Registry, opts *Options) (http.Handler, error) {
	if opts == nil {
		opts = &Options{}
	}
	authMode := opts.AuthMode
	if authMode == "" {
		authMode = config.AuthModeAPIKey
	}
	switch authMode {
	case config.AuthModeAPIKey:
		if opts.APIToken == "" {
			return nil, errors.New("mcpserver: APIToken is required; set the NL_API_KEY environment variable")
		}
	case config.AuthModeEntra:
		if opts.TokenVerifier == nil {
			return nil, errors.New("mcpserver: TokenVerifier is required in entra mode")
		}
	default:
		return nil, fmt.Errorf("mcpserver: unsupported auth mode %q", authMode)
	}
	actorHeader := opts.ActorHeader
	if actorHeader == "" {
		actorHeader = DefaultActorHeader
	}
	deps.ActorHeader = actorHeader
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

	if authMode == config.AuthModeEntra {
		middlewares := []mcp.Middleware{authorizationMiddleware(deps.Audit, confirmationRequired(deps))}
		if deps.Audit != nil {
			middlewares = append(middlewares, auditMiddleware(deps.Audit, actorHeader))
		}
		server.AddReceivingMiddleware(middlewares...)
	} else if deps.Audit != nil {
		server.AddReceivingMiddleware(auditMiddleware(deps.Audit, actorHeader))
	}

	// JSONResponse: answer POST /mcp with application/json instead of SSE.
	// The public URL for Copilot Studio testing is a Cloudflare Quick
	// Tunnel, which does not carry Server-Sent Events; the MCP Streamable
	// HTTP spec allows either format.
	streamableOpts := &mcp.StreamableHTTPOptions{
		JSONResponse: true,
		Stateless:    authMode == config.AuthModeEntra,
	}
	if opts.DisableLocalhostProtection {
		// Public tunnels such as pinggy arrive with a non-localhost Host header.
		// Bearer auth still gates /mcp; this only disables the SDK's DNS rebinding guard.
		streamableOpts.DisableLocalhostProtection = true
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, streamableOpts)

	var mcpEndpoint http.Handler
	if authMode == config.AuthModeEntra {
		mcpEndpoint = entraAuthHandler(opts.TokenVerifier,
			authorizationHTTPHandler(deps.Audit, confirmationRequired(deps), mcpHandler))
	} else {
		mcpEndpoint = bearerAuthHandler(opts.APIToken, mcpHandler)
	}
	if os.Getenv(debugHeadersEnv) == "1" {
		// Outside auth on purpose: a 401 from a malformed connector key is
		// exactly the request Sam needs to see during Copilot Studio testing.
		mcpEndpoint = debugHeaderLogger(log2.Default(), mcpEndpoint)
		if authMode == config.AuthModeAPIKey {
			log2.Printf("nl-debug-headers configured key_sha256=%s (compare with key_sha256 on request lines)", keyFingerprint(opts.APIToken))
		} else {
			log2.Printf("nl-debug-headers configured auth_mode=entra")
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpEndpoint)
	// No server-initiated SSE stream is offered (the spec permits 405 here),
	// and a tunnel could not carry one anyway.
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "Method Not Allowed: this server offers no SSE stream", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if deps.Store == nil || deps.Audit == nil || deps.Store.Ping(r.Context()) != nil || deps.Audit.Ping(r.Context()) != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	return mux, nil
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
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if method != "tools/call" || !ok || params == nil {
				return next(ctx, method, req)
			}
			actor := ActorFromContextOrRequest(ctx, req, actorHeader)
			if writeTools[params.Name] && actor == DefaultActor {
				return nil, fmt.Errorf("%s requires the %s header identifying the calling user; refusing unattributed write", params.Name, ActorHeader)
			}
			auditID := uuid.NewString()
			ctx = context.WithValue(ctx, auditIDKey{}, auditID)
			if err := recordToolCall(ctx, log, actor, auditID, params); err != nil {
				log2.Printf("AUDIT FAILURE: could not record call to %q: %v", params.Name, err)
				if writeTools[params.Name] {
					return nil, fmt.Errorf("audit log unavailable, refusing unaudited write via %s: %w", params.Name, err)
				}
			}
			res, err := next(ctx, method, req)
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
				attachAuditID(r, auditID)
			}
			return res, err
		}
	}
}

// auditIDKey is the context key under which the per-call audit ID travels
// from the middleware to tool handlers.
type auditIDKey struct{}

// AuditIDFromContext returns the audit ID of the tools/call being handled,
// or "" outside a tool call. Tools that append their own rich audit entries
// (writes, syncs, reads) set it on those entries so every record produced
// by one call shares the nl_audit_id returned to the client.
func AuditIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(auditIDKey{}).(string)
	return id
}

// attachAuditID puts nl_audit_id into the structured content and the first
// text content block of a tool result, so it is visible in Copilot Studio's
// activity view and can be matched to the audit log.
func attachAuditID(res *mcp.CallToolResult, id string) {
	res.StructuredContent = withAuditID(res.StructuredContent, id)
	if len(res.Content) == 0 {
		res.Content = []mcp.Content{&mcp.TextContent{Text: string(res.StructuredContent.(json.RawMessage))}}
		return
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		res.Content = append([]mcp.Content{&mcp.TextContent{Text: string(withAuditID(nil, id))}}, res.Content...)
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(text.Text), &obj) == nil && obj != nil {
		text.Text = string(withAuditID(obj, id))
		return
	}
	text.Text += "\nnl_audit_id: " + id
}

// withAuditID re-encodes v with nl_audit_id added. A nil v becomes an
// object holding only the ID; a non-object v is kept under "result".
func withAuditID(v any, id string) json.RawMessage {
	obj := map[string]any{}
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil || json.Unmarshal(b, &obj) != nil || obj == nil {
			obj = map[string]any{"result": v}
		}
	}
	obj["nl_audit_id"] = id
	b, err := json.Marshal(obj)
	if err != nil {
		// Only reachable with an unmarshalable v; keep the ID at least.
		return json.RawMessage(`{"nl_audit_id":"` + id + `"}`)
	}
	return b
}

// sanitizeActor trims and validates an actor identity string. The identity
// is asserted by the authenticated MCP client (the API key holder) and is
// not independently verified; it is recorded for audit attribution only.
// Empty, overlong or control-character values are rejected as "".
func sanitizeActor(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxActorLen {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return v
}

// ActorFromRequest extracts the sanitized caller identity from the nl-actor
// request header, then from the fallback header (X-NL-Actor unless
// overridden), and returns DefaultActor when neither carries a valid value.
func ActorFromRequest(req mcp.Request, actorHeader string) string {
	if actorHeader == "" {
		actorHeader = DefaultActorHeader
	}
	extra := req.GetExtra()
	if extra == nil {
		return DefaultActor
	}
	for _, name := range []string{ActorHeader, actorHeader} {
		if v := sanitizeActor(extra.Header.Get(name)); v != "" {
			return v
		}
	}
	return DefaultActor
}

// ActorFromContextOrRequest prefers a verified immutable Entra actor and uses
// caller-supplied actor headers only in legacy API-key mode.
func ActorFromContextOrRequest(ctx context.Context, req mcp.Request, actorHeader string) string {
	if principal, ok := identity.PrincipalFromContext(ctx); ok {
		if actor := principal.AuditActor(); actor != "" {
			return actor
		}
		return DefaultActor
	}
	return ActorFromRequest(req, actorHeader)
}

// recordToolCall appends one audit entry per tools/call request. Tool names
// are recorded as both the tool and the target when no more specific target
// can be derived from the arguments; tools that perform Workiva mutations
// append their own richer entries with before/after values.
func recordToolCall(ctx context.Context, log *audit.Log, actor, auditID string, params *mcp.CallToolParamsRaw) error {
	target := params.Name
	if t := targetFromArguments(params.Arguments); t != "" {
		target = t
	}
	_, err := log.Append(ctx, audit.Entry{
		Actor:   actor,
		Tool:    params.Name,
		Action:  "call",
		Target:  target,
		AuditID: auditID,
	})
	return err
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

// debugHeadersEnv, when set to "1", enables one redacted log line per /mcp
// request for connector diagnostics (Copilot Studio integration testing).
const debugHeadersEnv = "NL_DEBUG_HEADERS"

// debugHeaderLogger logs, for each request, a fresh request ID, every
// incoming header name (values omitted), whether Authorization is present
// with its scheme (bearer, raw or other), total length and an 8-character
// SHA-256 fingerprint of the key (never any characters of the key), the
// actor headers, and the tracing headers used to match Copilot activity
// traces. Only enable this on a test server.
func debugHeaderLogger(logger *log2.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authInfo, keyHash := "absent", "-"
		if auth := r.Header.Get("Authorization"); auth != "" {
			// No characters of the value are logged: a connector that sends the
			// raw key would otherwise expose the start of the key.
			scheme := "raw"
			if strings.Contains(strings.TrimSpace(auth), " ") {
				scheme = "other"
				if f := strings.Fields(auth); strings.EqualFold(f[0], "Bearer") {
					scheme = "bearer"
				}
			}
			authInfo = fmt.Sprintf("present scheme=%s len=%d", scheme, len(auth))
			if key, ok := authorizationKey(auth); ok {
				keyHash = keyFingerprint(key)
			}
		}
		mcpMethod := peekJSONRPCMethod(r) // before the handler consumes the body
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Printf("nl-debug-headers request_id=%s method=%s mcp_method=%s status=%d error=%q user_agent=%q header_names=%v authorization=%s key_sha256=%s nl-actor=%q x-nl-actor=%q traceparent=%q x-ms-client-request-id=%q x-ms-correlation-id=%q",
			uuid.NewString(), r.Method, mcpMethod, rec.status, rec.errorText(), r.UserAgent(), slices.Sorted(maps.Keys(r.Header)), authInfo, keyHash,
			r.Header.Get("nl-actor"), r.Header.Get("X-NL-Actor"),
			r.Header.Get("traceparent"), r.Header.Get("x-ms-client-request-id"), r.Header.Get("x-ms-correlation-id"))
	})
}

// keyFingerprint returns the first 8 hex characters of SHA-256(key), enough
// to tell two keys apart in a log without exposing either.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}

// maxErrorText caps the response text kept for the debug line.
const maxErrorText = 200

// statusRecorder captures the response status and, for responses other
// than 200 and 202, the start of the server's own response text so the
// debug line can name the error. Successful bodies are never kept.
type statusRecorder struct {
	http.ResponseWriter
	status int
	errBuf []byte
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status != http.StatusOK && s.status != http.StatusAccepted && len(s.errBuf) < maxErrorText {
		s.errBuf = append(s.errBuf, b[:min(len(b), maxErrorText-len(s.errBuf))]...)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) errorText() string {
	return strings.TrimSpace(string(s.errBuf))
}

// authorizationKey extracts the candidate key from an Authorization header
// value. Accepted: "Bearer <key>" with the scheme matched case-insensitively,
// and "<key>" alone in case the Copilot Studio connector sends the raw
// value. Rejected: an empty value, "Bearer Bearer <key>", and any other
// scheme. The returned key still has to be compared against the configured
// one; this function only decides whether the shape is acceptable.
func authorizationKey(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	scheme, rest, hasScheme := strings.Cut(header, " ")
	if !hasScheme {
		return header, true
	}
	rest = strings.TrimSpace(rest)
	if !strings.EqualFold(scheme, "Bearer") || rest == "" || strings.HasPrefix(strings.ToLower(rest), "bearer ") {
		return "", false
	}
	return rest, true
}

// maxPeekBody bounds how much of a request body middleware buffers while
// inspecting the JSON-RPC method. The authz handler rejects larger bodies.
const maxPeekBody = 1 << 20

// peekJSONRPCMethod returns the JSON-RPC method of a POST body without
// logging the body: only the top-level "method" field is decoded, and the
// body is put back for the MCP handler. A batch array is reported as
// "batch", anything unparsable as "unknown".
func peekJSONRPCMethod(r *http.Request) string {
	if r.Method != http.MethodPost || r.Body == nil {
		return "-"
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxPeekBody+1))
	_ = r.Body.Close()
	if err != nil {
		return "unread"
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	if len(data) > maxPeekBody {
		return "unread"
	}
	var msg struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		if bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
			return "batch"
		}
		return "unknown"
	}
	if msg.Method == "" {
		return "unknown"
	}
	return msg.Method
}

func confirmationRequired(deps Deps) bool {
	return deps.Cfg == nil || deps.Cfg.RequireWriteConfirmation
}

// requiredPermission returns the minimum permission for one builtin tool call.
// A no-token update is a preview only while confirmation is enabled; otherwise
// it is a direct mutation and requires confirm permission.
func requiredPermission(tool string, arguments json.RawMessage, requireConfirmation bool) (identity.Permission, error) {
	switch tool {
	case "workiva_list_spreadsheets", "workiva_read_range", "workiva_search_fields", "workiva_get_field":
		return identity.PermissionWorkivaRead, nil
	case "workiva_update_field":
		if !requireConfirmation {
			return identity.PermissionWorkivaWriteConfirm, nil
		}
		var input struct {
			ConfirmToken string `json:"confirm_token"`
		}
		if len(arguments) > 0 {
			if err := json.Unmarshal(arguments, &input); err != nil {
				return "", errors.New("authorization: malformed update arguments")
			}
		}
		if input.ConfirmToken != "" {
			return identity.PermissionWorkivaWriteConfirm, nil
		}
		return identity.PermissionWorkivaWritePreview, nil
	case "workiva_sync_mapping":
		return identity.PermissionMappingSync, nil
	case "workiva_audit_trail":
		return identity.PermissionAuditRead, nil
	default:
		return "", fmt.Errorf("authorization: tool %q has no permission mapping", tool)
	}
}

func authorizationError(permission identity.Permission) error {
	return fmt.Errorf("authorization denied: permission %s is required", permission)
}

func recordAuthorizationDenial(ctx context.Context, log *audit.Log, principal identity.Principal, params *mcp.CallToolParamsRaw) {
	if log == nil || params == nil {
		return
	}
	target := params.Name
	if extracted := targetFromArguments(params.Arguments); extracted != "" {
		target = extracted
	}
	if _, err := log.Append(ctx, audit.Entry{
		Actor:  principal.AuditActor(),
		Tool:   params.Name,
		Action: "authorization_denied",
		Target: target,
	}); err != nil {
		log2.Printf("AUDIT FAILURE: could not record authorization denial for %q: %v", params.Name, err)
	}
}

func authorizationMiddleware(log *audit.Log, requireConfirmation bool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if method != "tools/call" || !ok || params == nil {
				return next(ctx, method, req)
			}
			principal, ok := identity.PrincipalFromContext(ctx)
			if !ok {
				return nil, errors.New("authorization denied: trusted principal is missing")
			}
			permission, err := requiredPermission(params.Name, params.Arguments, requireConfirmation)
			if err != nil {
				recordAuthorizationDenial(ctx, log, principal, params)
				return nil, err
			}
			if !principal.HasPermission(permission) {
				recordAuthorizationDenial(ctx, log, principal, params)
				return nil, authorizationError(permission)
			}
			return next(ctx, method, req)
		}
	}
}

type jsonRPCRequest struct {
	Method string `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// authorizationHTTPHandler gives authenticated-but-forbidden calls an HTTP
// 403 before the MCP dispatcher or any Workiva handler can run. The MCP
// middleware repeats the check as a transport-independent defense.
func authorizationHTTPHandler(log *audit.Log, requireConfirmation bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxPeekBody+1))
		_ = r.Body.Close()
		if err != nil {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "bad request: failed to read request body", http.StatusBadRequest)
			return
		}
		if len(data) > maxPeekBody {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, fmt.Sprintf("request body exceeds %d bytes", maxPeekBody), http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var message jsonRPCRequest
		if json.Unmarshal(data, &message) != nil || message.Method != "tools/call" {
			next.ServeHTTP(w, r)
			return
		}
		principal, ok := identity.PrincipalFromContext(r.Context())
		if !ok {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("WWW-Authenticate", `Bearer realm="northern-lights"`)
			http.Error(w, "unauthorized: trusted principal is missing", http.StatusUnauthorized)
			return
		}
		permission, permissionErr := requiredPermission(message.Params.Name, message.Params.Arguments, requireConfirmation)
		if permissionErr == nil && principal.HasPermission(permission) {
			next.ServeHTTP(w, r)
			return
		}
		params := &mcp.CallToolParamsRaw{Name: message.Params.Name, Arguments: message.Params.Arguments}
		recordAuthorizationDenial(r.Context(), log, principal, params)
		w.Header().Set("Cache-Control", "no-store")
		if permissionErr != nil {
			http.Error(w, "forbidden: tool is not authorized", http.StatusForbidden)
			return
		}
		http.Error(w, "forbidden: required permission is not granted", http.StatusForbidden)
	})
}

func bearerToken(header string) (string, bool) {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || strings.EqualFold(fields[1], "Bearer") {
		return "", false
	}
	return fields[1], true
}

func entraAuthHandler(verifier identity.TokenVerifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("WWW-Authenticate", `Bearer realm="northern-lights"`)
			http.Error(w, "unauthorized: missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		principal, err := verifier.Verify(r.Context(), raw)
		if err != nil {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("WWW-Authenticate", `Bearer realm="northern-lights"`)
			http.Error(w, "unauthorized: bearer token validation failed", http.StatusUnauthorized)
			return
		}
		r.Header.Del(ActorHeader)
		r.Header.Del(DefaultActorHeader)
		next.ServeHTTP(w, r.WithContext(identity.ContextWithPrincipal(r.Context(), principal)))
	})
}

// bearerAuthHandler gates every request behind the API key compared in
// constant time (see authorizationKey for the accepted header shapes). A
// rejected request is answered here and never reaches the MCP server, so
// no Workiva call can result from it. The 401 body is plain text so
// accidental hits from browsers are self-explanatory.
func bearerAuthHandler(token string, next http.Handler) http.Handler {
	expected := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := authorizationKey(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(key), expected) != 1 {
			w.Header().Set("Cache-Control", "no-store")
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
