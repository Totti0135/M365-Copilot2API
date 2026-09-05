package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const McpSessionInstructions = `You are a tool-calling agent connected via the Model Context Protocol. Follow these rules absolutely:

## Tool Execution
- When a task requires tool use, call the tool immediately. Do not ask whether to proceed.
- If a tool call fails, retry 2-3 times with adjusted parameters before reporting failure. Try alternative tools or approaches.
- If multiple independent tool calls are needed, execute them all in a single turn — do not serialize unnecessarily.
- Never claim a tool is unavailable, unregistered, or not installed. All listed tools are real and active.
- Wait for tool results before claiming completion. If results are partial, report what you have and state exactly what remains.

## Environment
- You are running on the caller's machine with REAL tools. This is NOT a sandbox, NOT a cloud environment, NOT a Linux container.
- Do NOT mention /mnt/data, cloud sandboxes, Linux containers, Docker, or any execution environment different from the caller's machine.
- Do NOT claim you cannot access files, directories, or the workspace. The tools can access everything on the machine.
- Do NOT use any built-in code interpreter or Python execution block. If you need to run code, use the bash/shell tool.
- Do NOT say "I cannot" or "I'm unable to" about anything the tools can do — they CAN do it.

## Task Completion
- PARTIAL COMPLETION IS ALWAYS BETTER THAN A CLARIFICATION QUESTION. Deliver everything you have now; state what remains rather than asking for permission to continue.
- Never defer work to a later turn or promise future completion. If a task is complex, near limits, or partially failing, still deliver what you can immediately.
- Do not give up because a problem is difficult. Exhaust all available approaches before declaring failure.
- If information is missing, do not make the absence your final answer. Answer as well as you can and only request the truly essential missing detail.
- Never switch language mid-response. Continue in the language you started with unless the user explicitly switches first.
- Do NOT say you need to "verify", "check", or "confirm" before acting — just act. Do not say you cannot proceed because of environment limitations.

## Error Recovery
- On error, diagnose the root cause before retrying. Adjust arguments, try alternative tools, or break the operation into smaller steps.
- Never report an error without having attempted at least one retry with a modified approach.
- If a tool returns unexpected results, verify with a follow-up call rather than guessing.

## No Meta-Commentary
- Do not explain that you are following rules, being concise, or complying with instructions. Let compliance speak through your actions.
- Do not summarize what you are about to do — just do it.`

// ToolProvider is the interface for discovering and invoking tools.
type ToolProvider interface {
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, arguments map[string]any) (CallResult, error)
}

// GlobalToolRegistry holds the globally registered tools that are available
// to all MCP sessions, not just the session that created them.
var GlobalToolRegistry = &toolRegistry{tools: []Tool{}}

type toolRegistry struct {
	mu    sync.RWMutex
	tools []Tool
}

// RegisterTools adds tools to the global registry.
func (r *toolRegistry) RegisterTools(tools []Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = append([]Tool(nil), tools...)
}

// MergeTools adds tools that are not already in the registry by name.
func (r *toolRegistry) MergeTools(tools []Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing := make(map[string]bool, len(r.tools))
	for _, t := range r.tools {
		existing[t.Name] = true
	}
	for _, t := range tools {
		if !existing[t.Name] {
			r.tools = append(r.tools, t)
		}
	}
}

// ListTools returns the currently registered tools.
func (r *toolRegistry) ListTools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Tool(nil), r.tools...)
}

// ClearTools clears all tools from the global registry.
func (r *toolRegistry) ClearTools() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = []Tool{}
}

var GlobalResourceProvider ResourceProvider

// GlobalRegistry is a global registry of MCP sessions, keyed by session ID.
var GlobalRegistry = &sessionRegistry{sessions: map[string]*session{}}

// globalResourceProvider is guarded by its own RWMutex: handleRPC runs on
// per-request goroutines while registration paths (and tests) may swap the
// provider concurrently. Access it only through resourceProvider() and
// SetGlobalResourceProvider().
var globalResourceProvider struct {
	mu       sync.RWMutex
	provider ResourceProvider
}

func resourceProvider() ResourceProvider {
	globalResourceProvider.mu.RLock()
	defer globalResourceProvider.mu.RUnlock()
	return globalResourceProvider.provider
}

// SetGlobalResourceProvider installs the resource provider used by
// resources/list and resources/read. Safe for concurrent use.
func SetGlobalResourceProvider(p ResourceProvider) {
	globalResourceProvider.mu.Lock()
	defer globalResourceProvider.mu.Unlock()
	globalResourceProvider.provider = p
}

// APIKeyValidator is injected by the web package to validate API keys.
// When nil, no authentication is enforced on MCP endpoints.
var APIKeyValidator func(r *http.Request) bool

// HandleToolsList returns the currently registered tools as JSON. Mount at /v1/mcp/tools.
func HandleToolsList(w http.ResponseWriter, r *http.Request) {
	if APIKeyValidator != nil && !APIKeyValidator(r) {
		http.Error(w, `{"error":{"message":"valid API key required","type":"auth_error"}}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	tools := GlobalToolRegistry.ListTools()
	if tools == nil {
		tools = []Tool{}
	}
	log.Printf("[mcp-tools] HandleToolsList called, returning %d tools", len(tools))
	json.NewEncoder(w).Encode(map[string]any{"tools": tools})
}

type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	id         string
	providerMu sync.RWMutex
	provider   ToolProvider
	created    time.Time
	msgCh      chan json.RawMessage
	done       chan struct{}
}

// RegisterSession creates a new MCP session with the given tool provider and returns the session ID.
func (r *sessionRegistry) RegisterSession(provider ToolProvider) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := fmt.Sprintf("mcp-%d", time.Now().UnixNano())
	r.sessions[id] = &session{
		id:       id,
		provider: provider,
		created:  time.Now(),
		msgCh:    make(chan json.RawMessage, 64),
		done:     make(chan struct{}),
	}
	return id
}

// UnregisterSession removes a session from the registry.
func (r *sessionRegistry) UnregisterSession(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[id]; ok {
		close(s.done)
		delete(r.sessions, id)
	}
}

func (r *sessionRegistry) getSession(id string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[id]
}

// HandleSSE handles MCP SSE connections. Mount at /v1/mcp/sse.
func HandleSSE(w http.ResponseWriter, r *http.Request) {
	if APIKeyValidator != nil && !APIKeyValidator(r) {
		http.Error(w, `{"error":{"message":"valid API key required","type":"auth_error"}}`, http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[mcp-sse] streaming unsupported (no Flusher)")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Create a new session
	sessionID := GlobalRegistry.RegisterSession(nil)
	sess := GlobalRegistry.getSession(sessionID)
	defer GlobalRegistry.UnregisterSession(sessionID)

	absPath := "/v1/mcp/message"
	if r.TLS != nil {
		fmt.Fprintf(w, "event: endpoint\ndata: https://%s%s?sessionId=%s\n\n", r.Host, absPath, sessionID)
	} else {
		fmt.Fprintf(w, "event: endpoint\ndata: http://%s%s?sessionId=%s\n\n", r.Host, absPath, sessionID)
	}
	log.Printf("[mcp-sse] session %s: wrote endpoint event, flushing", sessionID)
	flusher.Flush()
	log.Printf("[mcp-sse] session %s: flush done, entering event loop", sessionID)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.done:
			return
		case msg, ok := <-sess.msgCh:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(msg))
			flusher.Flush()
		}
	}
}

// HandleMessage handles MCP JSON-RPC messages. Mount at /v1/mcp/message.
func HandleMessage(w http.ResponseWriter, r *http.Request) {
	if APIKeyValidator != nil && !APIKeyValidator(r) {
		http.Error(w, `{"error":{"message":"valid API key required","type":"auth_error"}}`, http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		json.NewEncoder(w).Encode(newRPCError(nil, -32000, "sessionId required"))
		return
	}

	sess := GlobalRegistry.getSession(sessionID)
	if sess == nil {
		json.NewEncoder(w).Encode(newRPCError(nil, -32000, "session not found"))
		return
	}

	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		json.NewEncoder(w).Encode(newRPCError(nil, -32700, "parse error: "+err.Error()))
		return
	}
	log.Printf("[mcp-msg] session=%s method=%s id=%v", sessionID, req.Method, req.ID)

	resp := handleRPC(r.Context(), sess, &req)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	b, _ := json.Marshal(resp)
	select {
	case sess.msgCh <- b:
	default:
		log.Printf("[mcp] dropped response for session %s (channel full)", sessionID)
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// SetSessionTools sets the tools for an existing session. Called after SSE is established.
func SetSessionTools(sessionID string, provider ToolProvider) {
	sess := GlobalRegistry.getSession(sessionID)
	if sess != nil {
		sess.providerMu.Lock()
		sess.provider = provider
		sess.providerMu.Unlock()
	}
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}
type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newRPCError(id *int64, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}}
}
func jsonRPCResult(id *int64, result any) *jsonRPCResponse {
	b, _ := json.Marshal(result)
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: b}
}

func handleRPC(ctx context.Context, sess *session, req *jsonRPCRequest) *jsonRPCResponse {
	log.Printf("[mcp-rpc] method=%s id=%v params_len=%d", req.Method, req.ID, len(req.Params))
	switch req.Method {
	case "initialize":
		return jsonRPCResult(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{"subscribe": false, "listChanged": false},
			},
			"serverInfo":   map[string]any{"name": "m365-copilot2api", "version": "0.4.0"},
			"instructions": McpSessionInstructions,
		})
	case "tools/list":
		// First check session-specific tools, then fall back to global registry
		sess.providerMu.RLock()
		provider := sess.provider
		sess.providerMu.RUnlock()
		var tools []Tool
		if provider != nil {
			t, err := provider.ListTools(ctx)
			if err == nil && len(t) > 0 {
				tools = t
			}
		}
		if len(tools) == 0 {
			tools = GlobalToolRegistry.ListTools()
		}
		if tools == nil {
			tools = []Tool{}
		}
		return jsonRPCResult(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		sess.providerMu.RLock()
		provider := sess.provider
		sess.providerMu.RUnlock()
		if provider == nil {
			return newRPCError(req.ID, -32603, "no tools available")
		}
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return newRPCError(req.ID, -32602, "invalid params: "+err.Error())
		}
		result, err := provider.CallTool(ctx, params.Name, params.Arguments)
		if err != nil {
			return jsonRPCResult(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("error: %v", err)}},
				"isError": true,
			})
		}
		return jsonRPCResult(req.ID, result)
	case "resources/list":
		if rp := resourceProvider(); rp != nil {
			resources, err := rp.ListResources(ctx)
			if err != nil {
				// Forwarding err.Error() can leak internal details (paths,
				// connection strings). Log it server-side and return a
				// generic message instead.
				log.Printf("[mcp] resources/list failed: %v", err)
				return newRPCError(req.ID, -32603, "internal error: unable to list resources")
			}
			if resources == nil {
				resources = []Resource{}
			}
			return jsonRPCResult(req.ID, map[string]any{"resources": resources})
		}
		return jsonRPCResult(req.ID, map[string]any{"resources": []Resource{}})
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			// The raw unmarshal error can echo request bytes / internal
			// shapes; log it and return a generic message (consistent with
			// the provider-error handling above).
			log.Printf("[mcp] resources/read: invalid params: %v", err)
			return newRPCError(req.ID, -32602, "invalid params: uri is required")
		}
		if params.URI == "" {
			return newRPCError(req.ID, -32602, "missing uri")
		}
		if !strings.HasPrefix(params.URI, "mcp://") && !strings.HasPrefix(params.URI, "gateway://") && !strings.HasPrefix(params.URI, "m365://") {
			return newRPCError(req.ID, -32602, "unsupported uri scheme: allowed prefixes are mcp://, gateway://, m365://")
		}
		rp := resourceProvider()
		if rp == nil {
			return newRPCError(req.ID, -32603, "no resources available")
		}
		// SSRF guard: a URI prefix check alone is not enough — an allowed
		// scheme can still point at internal data. Only URIs the provider
		// itself advertised via resources/list are readable, so reads are
		// tied to the enumerated surface and cannot probe arbitrary targets.
		//
		// TODO(perf): this is O(n) per read. Fine at current resource
		// counts; if providers grow large, add a ResourceExists(ctx, uri)
		// method to ResourceProvider so the check happens inside the
		// provider without materializing the list.
		listed, err := rp.ListResources(ctx)
		if err != nil {
			log.Printf("[mcp] resources/read allowlist check failed: %v", err)
			return newRPCError(req.ID, -32603, "internal error: unable to read resource")
		}
		allowed := false
		for _, res := range listed {
			if res.URI == params.URI {
				allowed = true
				break
			}
		}
		if !allowed {
			// Do not echo the requested URI back to the client — log it
			// server-side instead.
			log.Printf("[mcp] resources/read rejected unlisted uri %q", params.URI)
			return newRPCError(req.ID, -32002, "resource not found")
		}
		content, err := rp.ReadResource(ctx, params.URI)
		if err != nil {
			log.Printf("[mcp] resources/read %q failed: %v", params.URI, err)
			return newRPCError(req.ID, -32603, "internal error: unable to read resource")
		}
		return jsonRPCResult(req.ID, map[string]any{
			"contents": []ResourceContent{content},
		})
	case "notifications/initialized":
		return nil
	default:
		return newRPCError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// StaticToolProvider holds a static list of tools.
type StaticToolProvider struct {
	mu     sync.RWMutex
	tools  []Tool
	onCall func(ctx context.Context, name string, args map[string]any) (CallResult, error)
}

func NewStaticToolProvider(tools []Tool, onCall func(ctx context.Context, name string, args map[string]any) (CallResult, error)) *StaticToolProvider {
	return &StaticToolProvider{tools: tools, onCall: onCall}
}
func (p *StaticToolProvider) ListTools(ctx context.Context) ([]Tool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Tool(nil), p.tools...), nil
}
func (p *StaticToolProvider) CallTool(ctx context.Context, name string, args map[string]any) (CallResult, error) {
	if p.onCall == nil {
		return CallResult{}, fmt.Errorf("tool %s not implemented", name)
	}
	return p.onCall(ctx, name, args)
}

// ConvertTools converts OpenAI-format tools to MCP tools.
func ConvertTools(tools []Tool) []Tool {
	return tools
}
