package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionBinding records a reusable content-keyed conversation. Identity
// fields are diagnostic metadata; Resolve matches only conversation content.
type sessionBinding struct {
	SessionID      string `json:"sessionId"`
	ConversationID string `json:"conversationId"`
	AccountID      string `json:"accountId"`
	// APIKeyID 记录最近一次发起请求的 API Key（JWT/未知 key 为空，不抹掉旧值），
	// 供控制台按 key 筛选对话。
	APIKeyID      string    `json:"apiKeyId,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUsedAt    time.Time `json:"lastUsedAt"`
	IPFingerprint string    `json:"ipFingerprint,omitempty"`
	UserField     string    `json:"userField,omitempty"`
	ContextFinger string    `json:"contextFinger,omitempty"`
	// ContextHistory 鎸佷箙鍖栦繚瀛樻渶杩戜竴娆″崗璁殑瀹屾暣娑堟伅锛屼緵閲嶅惎鍚庣户缁仛
	// 鍐呭鍓嶇紑鍖归厤锛岄伩鍏嶈繘绋嬮噸鍚鑷存墍鏈変細璇濋敭鍏ㄩ儴澶辨晥銆?
	ContextHistory []oaiMsg `json:"contextHistory,omitempty"`
	// Tenant isolates a binding to the API key that created it. Every read,
	// match, resume, and delete is scoped to the caller's tenant so one key can
	// never touch another key's conversations. An empty tenant marks a legacy
	// binding (created before this field existed): it is treated as unowned and
	// is never returned to a keyed caller.
	Tenant string `json:"tenant,omitempty"`
	// ExplicitID is the client-supplied X-M365-Session-Id. It is namespaced per
	// tenant via byExplicit so two tenants may use the same id without colliding.
	ExplicitID string `json:"explicitId,omitempty"`
}

type sessionResolver struct {
	mu          sync.Mutex
	path        string
	sessions    map[string]sessionBinding
	byExplicit  map[string]string // explicitID -> sessionID
	byUserField map[string]string // userField -> sessionID
	byIPFinger  map[string]string // ipFingerprint -> sessionID
	byContext   map[string]string // contextFingerprint -> sessionID
	ttl         time.Duration
	contextTTL  time.Duration
	maxSessions int
	persist     *persistStore
}

const defaultMaxSessions = 1000

func openSessionResolver() *sessionResolver {
	// Treat a session as expired after two idle hours. Expired bindings are
	// removed from sessions.json; cloud conversations use the same cleanup window.
	ttl := 2 * time.Hour
	if v := os.Getenv("M365_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			ttl = d
		}
	}
	contextTTL := 2 * time.Hour
	if v := os.Getenv("M365_CONTEXT_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			contextTTL = d
		}
	}
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = "sessions.json"
	}
	sr := &sessionResolver{
		path:        path,
		sessions:    map[string]sessionBinding{},
		byExplicit:  map[string]string{},
		byUserField: map[string]string{},
		byIPFinger:  map[string]string{},
		byContext:   map[string]string{},
		ttl:         ttl,
		contextTTL:  contextTTL,
		maxSessions: defaultMaxSessions,
	}
	sr.persist = &persistStore{flush: sr.flush}
	sr.loadLocked()
	return sr
}

func (sr *sessionResolver) loadLocked() {
	if b, err := os.ReadFile(sr.path); err == nil {
		var list []sessionBinding
		if err := json.Unmarshal(b, &list); err == nil {
			now := time.Now().UTC()
			for _, s := range list {
				if now.Sub(s.LastUsedAt) > sr.ttl {
					continue
				}
				sr.reindexLocked(s)
			}
		}
	}
}

// flush 在锁内生成快照，锁外写盘。
func (sr *sessionResolver) flush() error {
	sr.mu.Lock()
	list := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		list = append(list, s)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	sr.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(sr.path, b, 0o600)
}

func (sr *sessionResolver) reindexLocked(s sessionBinding) {
	if old, ok := sr.sessions[s.SessionID]; ok {
		if old.UserField != "" && sr.byUserField[old.UserField] == s.SessionID {
			delete(sr.byUserField, old.UserField)
		}
		if old.IPFingerprint != "" && sr.byIPFinger[old.IPFingerprint] == s.SessionID {
			delete(sr.byIPFinger, old.IPFingerprint)
		}
		if old.ContextFinger != "" && sr.byContext[old.ContextFinger] == s.SessionID {
			delete(sr.byContext, old.ContextFinger)
		}
	}
	sr.sessions[s.SessionID] = s
	if s.ExplicitID != "" {
		sr.byExplicit[explicitKey(s.Tenant, s.ExplicitID)] = s.SessionID
	}
	if s.UserField != "" {
		sr.byUserField[s.UserField] = s.SessionID
	}
	if s.IPFingerprint != "" {
		sr.byIPFinger[s.IPFingerprint] = s.SessionID
	}
	if s.ContextFinger != "" {
		sr.byContext[s.ContextFinger] = s.SessionID
	}
}

func (sr *sessionResolver) evictLocked() {
	now := time.Now().UTC()
	for id, s := range sr.sessions {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			sr.dropLocked(id, s)
		}
	}
	if len(sr.sessions) > sr.maxSessions {
		// Bound memory by dropping the least recently used sessions.
		ids := make([]string, 0, len(sr.sessions))
		last := make(map[string]time.Time, len(sr.sessions))
		for id, s := range sr.sessions {
			ids = append(ids, id)
			last[id] = s.LastUsedAt
		}
		sort.Slice(ids, func(i, j int) bool { return last[ids[i]].Before(last[ids[j]]) })
		for _, id := range ids[:len(sr.sessions)-sr.maxSessions] {
			sr.dropLocked(id, sr.sessions[id])
		}
	}
}

func (sr *sessionResolver) dropLocked(id string, s sessionBinding) {
	delete(sr.sessions, id)
	if s.ExplicitID != "" {
		delete(sr.byExplicit, explicitKey(s.Tenant, s.ExplicitID))
	}
	if sr.byUserField[s.UserField] == id {
		delete(sr.byUserField, s.UserField)
	}
	if sr.byIPFinger[s.IPFingerprint] == id {
		delete(sr.byIPFinger, s.IPFingerprint)
	}
	if sr.byContext[s.ContextFinger] == id {
		delete(sr.byContext, s.ContextFinger)
	}
}

type ResolveResult struct {
	SessionID      string
	ConversationID string
	AccountID      string
	MatchedBy      string
	IsNew          bool
	// HistoryLen is the number of messages already present in a reused cloud
	// conversation and therefore the start index for incremental delivery.
	HistoryLen int
}

func clientIPFingerprint(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ua := r.Header.Get("User-Agent")
	data := host + "|" + ua
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func contextFingerprint(messages []oaiMsg) string {
	if len(messages) == 0 {
		return ""
	}
	var parts []string
	limit := len(messages)
	if limit > 3 {
		limit = 3
	}
	for i := len(messages) - limit; i < len(messages); i++ {
		m := messages[i]
		parts = append(parts, m.Role+":"+contentToString(m.Content))
	}
	data := strings.Join(parts, "||")
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func (sr *sessionResolver) Resolve(r *http.Request, body *oaiReq) ResolveResult {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	tenant := tenantFromRequest(r)
	explicitID := r.Header.Get("X-M365-Session-Id")

	// An explicit client session ID has the highest continuation priority. The
	// caller determines which cloud conversation should continue.
	if explicitID != "" {
		if sessID, ok := sr.byExplicit[explicitKey(tenant, explicitID)]; ok {
			if sess, ok := sr.sessions[sessID]; ok && sess.Tenant == tenant {
				sess.LastUsedAt = time.Now().UTC()
				sr.sessions[sessID] = sess
				sr.persist.markDirty()
				return ResolveResult{
					SessionID:      sess.SessionID,
					ConversationID: sess.ConversationID,
					AccountID:      sess.AccountID,
					MatchedBy:      "explicit",
					IsNew:          false,
					HistoryLen:     len(sess.ContextHistory),
				}
			}
		}
	}

	// Reuse a cloud conversation when its recorded history is a strict prefix
	// of the request under the same IP and user-agent fingerprint. HistoryLen
	// identifies the incremental suffix to send.
	ipFinger := clientIPFingerprint(r)
	if bestID, n := sr.matchContextLocked(tenant, ipFinger, body.Messages); bestID != "" {
		sess := sr.sessions[bestID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_prefix_%d", n),
			IsNew:          false,
			HistoryLen:     n,
		}
	}

	// As a weaker fallback, reuse a conversation whose history shares a strong
	// suffix with a locally truncated request. The incremental boundary is unknown.
	suffixID, suffixN := sr.matchSuffixLocked(tenant, ipFinger, body.Messages)
	if suffixID != "" {
		sess := sr.sessions[suffixID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[suffixID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_suffix_%d", suffixN),
			IsNew:          false,
			HistoryLen:     suffixN,
		}
	}

	return ResolveResult{IsNew: true}
}

func (sr *sessionResolver) matchSuffixLocked(tenant, ipFinger string, messages []oaiMsg) (string, int) {
	if len(messages) < 2 {
		return "", 0
	}
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	minSuffix := 2
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.Tenant != tenant {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		hist := sess.ContextHistory
		if len(hist) < minSuffix {
			continue
		}
		n := suffixMatchLen(hist, messages)
		if n >= minSuffix && (n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent))) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

func suffixMatchLen(hist, msgs []oaiMsg) int {
	maxN := len(hist)
	if maxN > len(msgs) {
		maxN = len(msgs)
	}
	n := 0
	for i := 1; i <= maxN; i++ {
		if messagesEqual(hist[len(hist)-i], msgs[len(msgs)-i]) {
			n = i
		} else {
			break
		}
	}
	return n
}

// matchContextLocked returns the most recent conversation with the longest
// strict history prefix, plus the number of matched messages.
func (sr *sessionResolver) matchContextLocked(tenant, ipFinger string, messages []oaiMsg) (string, int) {
	if len(messages) == 0 {
		return "", 0
	}
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.Tenant != tenant {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		n := contextPrefixLen(sess.ContextHistory, messages)
		if n >= 1 && (n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent))) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

// contextPrefixLen returns len(hist) when hist is a strict prefix of msgs and
// ends on an atom boundary. It returns zero otherwise.
func contextPrefixLen(hist, msgs []oaiMsg) int {
	if len(hist) == 0 || len(msgs) < len(hist) {
		return 0
	}
	for i := range hist {
		if !messagesEqual(hist[i], msgs[i]) {
			return 0
		}
	}
	atoms := buildAtoms(msgs)
	boundary := false
	for _, a := range atoms {
		if a.End == len(hist) {
			boundary = true
			break
		}
		if a.End > len(hist) {
			break
		}
	}
	if !boundary {
		return 0
	}
	return len(hist)
}

// messagesEqual compares role, content, and tool call semantics while ignoring
// tool call IDs, which clients may regenerate when replaying the same call.
func messagesEqual(a, b oaiMsg) bool {
	if a.Role != b.Role {
		return false
	}
	ta := contentToString(a.Content)
	tb := contentToString(b.Content)
	if ta != tb {
		return false
	}
	if (a.ToolCalls == nil) != (b.ToolCalls == nil) {
		return false
	}
	for i := range a.ToolCalls {
		if i >= len(b.ToolCalls) {
			return false
		}
		if toolCallEqual(a.ToolCalls[i], b.ToolCalls[i]) {
			continue
		}
		return false
	}
	return len(a.ToolCalls) == len(b.ToolCalls)
}

// toolCallEqual 比较 name 与 arguments，忽略 ID：同一段工具调用重放时
// ID 由客户端重新生成，不应影响会话键。
func toolCallEqual(x, y map[string]any) bool {
	xFunc, _ := x["function"].(map[string]any)
	yFunc, _ := y["function"].(map[string]any)
	xn, _ := xFunc["name"].(string)
	yn, _ := yFunc["name"].(string)
	if xn != yn {
		return false
	}
	xa, _ := xFunc["arguments"].(string)
	ya, _ := yFunc["arguments"].(string)
	return xa == ya
}

func (sr *sessionResolver) Bind(sessionID, conversationID, accountID, apiKeyID string, body *oaiReq, assistantText string, r *http.Request) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	tenant := tenantFromRequest(r)
	now := time.Now().UTC()
	history := cloneMessages(body.Messages)
	if strings.TrimSpace(assistantText) != "" {
		history = append(history, oaiMsg{Role: "assistant", Content: assistantText})
	}
	explicitID := r.Header.Get("X-M365-Session-Id")

	// Locate an existing binding to update in place, scoped to this tenant:
	// prefer the tenant-namespaced explicit id, then any binding this tenant
	// already holds for the same cloud conversation. This keeps one record per
	// conversation instead of growing sessions.json on every incremental turn,
	// and never merges into another tenant's binding.
	targetKey := ""
	if explicitID != "" {
		if k, ok := sr.byExplicit[explicitKey(tenant, explicitID)]; ok {
			if sess, ok := sr.sessions[k]; ok && sess.Tenant == tenant {
				targetKey = k
			}
		}
	}
	if targetKey == "" && conversationID != "" {
		for k, sess := range sr.sessions {
			if sess.Tenant == tenant && sess.ConversationID == conversationID {
				targetKey = k
				break
			}
		}
	}
	if targetKey != "" {
		sess := sr.sessions[targetKey]
		sess.ConversationID = conversationID
		sess.AccountID = accountID
		// 空 apiKeyID（JWT/未知 key）不覆盖已记录的归因，供控制台按 key 筛选对话。
		if apiKeyID != "" {
			sess.APIKeyID = apiKeyID
		}
		sess.LastUsedAt = now
		sess.UserField = body.User
		sess.IPFingerprint = clientIPFingerprint(r)
		sess.ContextFinger = contextFingerprint(history)
		sess.ContextHistory = history
		sess.Tenant = tenant
		if explicitID != "" {
			sess.ExplicitID = explicitID
		}
		sr.sessions[targetKey] = sess
		sr.reindexLocked(sess)
		sr.persist.markDirty()
		return
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	sess := sessionBinding{
		SessionID:      sessionID,
		ConversationID: conversationID,
		AccountID:      accountID,
		APIKeyID:       apiKeyID,
		CreatedAt:      now,
		LastUsedAt:     now,
		IPFingerprint:  clientIPFingerprint(r),
		UserField:      body.User,
		ContextFinger:  contextFingerprint(history),
		ContextHistory: history,
		Tenant:         tenant,
		ExplicitID:     explicitID,
	}
	sr.reindexLocked(sess)
	sr.persist.markDirty()
}

func (sr *sessionResolver) GetSession(tenant, sessionID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.lookupForTenantLocked(tenant, sessionID)
}

// lookupForTenantLocked resolves a client-facing id (either the raw SessionID
// or the tenant's explicit X-M365-Session-Id) to a binding this tenant owns.
func (sr *sessionResolver) lookupForTenantLocked(tenant, id string) (sessionBinding, bool) {
	if tenant == "" || id == "" {
		return sessionBinding{}, false
	}
	if s, ok := sr.sessions[id]; ok && s.Tenant == tenant {
		return s, true
	}
	if k, ok := sr.byExplicit[explicitKey(tenant, id)]; ok {
		if s, ok := sr.sessions[k]; ok && s.Tenant == tenant {
			return s, true
		}
	}
	return sessionBinding{}, false
}

// GetConversation resolves a cloud conversation binding. A keyed caller
// (tenant = key hash) is scoped to its own bindings; the console path
// (no API key, empty tenant) falls back to an ID-only match so the admin
// view can open any recorded conversation.
func (sr *sessionResolver) GetConversation(tenant, conversationID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for _, session := range sr.sessions {
		if session.ConversationID != conversationID {
			continue
		}
		if tenant != "" && session.Tenant != tenant {
			continue
		}
		session.ContextHistory = cloneMessages(session.ContextHistory)
		return session, true
	}
	return sessionBinding{}, false
}

func (sr *sessionResolver) ListSessions() []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

func (sr *sessionResolver) DeleteSession(tenant, sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sess, ok := sr.lookupForTenantLocked(tenant, sessionID)
	if !ok {
		return false
	}
	sr.dropLocked(sess.SessionID, sess)
	sr.persist.markDirty()
	return true
}

// UnbindByConversation drops every session bound to the given conversation.
// Called after an automatic cleanup deletes the cloud conversation, so the
// anti-CrossID resolver never reuses a dead conversation.
func (sr *sessionResolver) UnbindByConversation(conversationID string) int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	removed := 0
	for sid, s := range sr.sessions {
		if s.ConversationID != conversationID {
			continue
		}
		// dropLocked removes the binding and every derived index entry
		// (including the tenant-namespaced byExplicit key). This is a global
		// maintenance path: a deleted cloud conversation is unbound for every
		// tenant, so no tenant filter is applied here.
		sr.dropLocked(sid, s)
		removed++
	}
	if removed > 0 {
		sr.persist.markDirty()
	}
	return removed
}

func cloneMessages(msgs []oaiMsg) []oaiMsg {
	if len(msgs) <= 512 {
		out := make([]oaiMsg, len(msgs))
		copy(out, msgs)
		return out
	}
	atoms := buildAtoms(msgs)
	if len(atoms) == 0 {
		msgs = msgs[len(msgs)-512:]
		out := make([]oaiMsg, len(msgs))
		copy(out, msgs)
		return out
	}
	count := 0
	startIdx := len(msgs)
	for i := len(atoms) - 1; i >= 0; i-- {
		c := atoms[i].End - atoms[i].Start
		if count+c > 512 {
			break
		}
		count += c
		startIdx = atoms[i].Start
	}
	if count == 0 {
		startIdx = atoms[len(atoms)-1].Start
	}
	sliced := msgs[startIdx:]
	out := make([]oaiMsg, len(sliced))
	copy(out, sliced)
	for i := range out {
		if len(sliced[i].ToolCalls) > 0 {
			tc := make([]map[string]any, len(sliced[i].ToolCalls))
			for j := range sliced[i].ToolCalls {
				m := make(map[string]any, len(sliced[i].ToolCalls[j]))
				for k, v := range sliced[i].ToolCalls[j] {
					m[k] = v
				}
				tc[j] = m
			}
			out[i].ToolCalls = tc
		}
	}
	return out
}

func explicitKey(tenant, id string) string { return tenant + "\x00" + id }

// tenantFromRequest derives a stable, non-reversible tenant identifier from the
// caller's API key so per-caller session state is isolated. Returns "" when no
// key is present; an empty tenant never matches a stored (keyed) binding.
func tenantFromRequest(r *http.Request) string {
	raw := rawAPIKey(r)
	if raw == "" {
		return ""
	}
	return keyHash(raw)
}

// ListSessionsForTenant returns only the bindings owned by the given tenant,
// most-recently-used first. Used by the API-key-authenticated /v1/sessions
// endpoint; the global ListSessions is reserved for admin/maintenance paths.
func (sr *sessionResolver) ListSessionsForTenant(tenant string) []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0)
	if tenant == "" {
		return out
	}
	for _, s := range sr.sessions {
		if s.Tenant == tenant {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsedAt.After(out[j].LastUsedAt) })
	return out
}
