package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Access requests let a local app (usually a web page) ask the person at the
// keyboard for an API key without copying one by hand:
//
//  1. The app generates its own random key and calls POST /access/requests
//     with {"name", "key", "scopes"}. The engine answers with a request id.
//  2. A management client (the desktop app) long-polls
//     GET /v8/management/access/requests and asks the user.
//  3. Approving adds the key to access.api-keys and records a grant, so the
//     app can be listed and revoked later. The app learns the outcome from
//     GET /access/requests/:id and then uses its key as a normal API key.
//
// Requests are only accepted from loopback peers, and only while a
// management client is watching, so a plain server never queues prompts
// that nobody will see.
//
// Scopes name what an app may do. "models" (everything /v1 served before
// scopes existed) is always granted and not enforced; "web" unlocks the
// /v1/tools web search and page fetch endpoints. An app that already holds
// an approved key can ask for more scopes for that same key (an upgrade
// request); approving it updates the existing grant.

const (
	accessRequestTTL        = 5 * time.Minute
	accessResolvedRetention = 10 * time.Minute
	accessDenyCooldown      = 30 * time.Second
	accessWatcherWindow     = 75 * time.Second
	accessMaxPending        = 8
	accessMaxWait           = 55 * time.Second
	accessMinKeyLength      = 32
	accessMaxKeyLength      = 256
	accessMaxNameLength     = 64
	accessGrantsFileName    = "access-grants.json"
)

// Scopes an access request can ask for, in canonical order.
const (
	ScopeModels = "models"
	ScopeWeb    = "web"
)

// AllScopes lists every scope; keys without a grant (the user's own) have all of them.
var AllScopes = []string{ScopeModels, ScopeWeb}

// Access request states.
const (
	AccessPending  = "pending"
	AccessApproved = "approved"
	AccessDenied   = "denied"
	AccessExpired  = "expired"
)

type accessRequest struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Origin    string    `json:"origin"`
	UserAgent string    `json:"user_agent,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Scopes is what the app will have once approved.
	Scopes []string `json:"scopes"`
	// Upgrade marks a request for more scopes for an already granted key.
	Upgrade     bool     `json:"upgrade,omitempty"`
	GrantID     string   `json:"grant_id,omitempty"`
	AddedScopes []string `json:"added_scopes,omitempty"`

	key        string
	resolvedAt time.Time
}

// AccessGrant is an approved app. The key itself lives in access.api-keys;
// the grant only stores its hash so the file holds no secrets.
type AccessGrant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Origin    string    `json:"origin"`
	KeyHash   string    `json:"key_hash"`
	KeyHint   string    `json:"key_hint"`
	GrantedAt time.Time `json:"granted_at"`
	// Scopes the app holds; grants written before scopes existed mean ["models"].
	Scopes []string `json:"scopes"`
}

type accessBroker struct {
	mu        sync.Mutex
	requests  map[string]*accessRequest
	cooldown  map[string]time.Time // origin -> no new requests until
	lastWatch time.Time
	changed   chan struct{}

	grantsLoaded bool
	grants       []AccessGrant
}

func (h *Handler) access() *accessBroker {
	h.accessOnce.Do(func() {
		h.accessBroker = &accessBroker{
			requests: map[string]*accessRequest{},
			cooldown: map[string]time.Time{},
			changed:  make(chan struct{}),
		}
	})
	return h.accessBroker
}

// notifyLocked wakes every long-poll waiter. Callers hold b.mu.
func (b *accessBroker) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// expireLocked marks stale pending requests expired and drops old resolved ones.
func (b *accessBroker) expireLocked(now time.Time) {
	changed := false
	for id, req := range b.requests {
		switch {
		case req.Status == AccessPending && now.After(req.ExpiresAt):
			req.Status = AccessExpired
			req.resolvedAt = now
			req.key = ""
			changed = true
		case req.Status != AccessPending && now.Sub(req.resolvedAt) > accessResolvedRetention:
			delete(b.requests, id)
		}
	}
	for origin, until := range b.cooldown {
		if now.After(until) {
			delete(b.cooldown, origin)
		}
	}
	if changed {
		b.notifyLocked()
	}
}

func (b *accessBroker) pendingLocked() []accessRequest {
	out := make([]accessRequest, 0)
	for _, req := range b.requests {
		if req.Status == AccessPending {
			copied := *req
			copied.Scopes = slices.Clone(req.Scopes)
			copied.AddedScopes = slices.Clone(req.AddedScopes)
			out = append(out, copied)
		}
	}
	slices.SortFunc(out, func(a, b accessRequest) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// waitDuration parses ?wait=<seconds>, capped at accessMaxWait.
func waitDuration(c *gin.Context) time.Duration {
	seconds, err := strconv.Atoi(c.Query("wait"))
	if err != nil || seconds <= 0 {
		return 0
	}
	wait := time.Duration(seconds) * time.Second
	if wait > accessMaxWait {
		wait = accessMaxWait
	}
	return wait
}

func isLoopbackPeer(c *gin.Context) bool {
	ip := net.ParseIP(c.RemoteIP())
	return ip != nil && ip.IsLoopback()
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func keyHint(key string) string {
	if len(key) <= 8 {
		return "…"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func validAccessKey(key string) bool {
	if len(key) < accessMinKeyLength || len(key) > accessMaxKeyLength {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// cleanName keeps printable characters and collapses whitespace.
func cleanName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.Join(strings.Fields(b.String()), " ")
	if runes := []rune(name); len(runes) > accessMaxNameLength {
		name = string(runes[:accessMaxNameLength])
	}
	return name
}

// normalizeScopes validates requested scope names and returns them in
// canonical order with "models" always included. A nil list means ["models"].
func normalizeScopes(requested []string) ([]string, string, bool) {
	want := map[string]bool{ScopeModels: true}
	for _, scope := range requested {
		scope = strings.TrimSpace(scope)
		if !slices.Contains(AllScopes, scope) {
			return nil, scope, false
		}
		want[scope] = true
	}
	out := make([]string, 0, len(want))
	for _, scope := range AllScopes {
		if want[scope] {
			out = append(out, scope)
		}
	}
	return out, "", true
}

// grantScopes returns a grant's scopes, defaulting old grants to ["models"].
func grantScopes(g AccessGrant) []string {
	scopes, _, ok := normalizeScopes(g.Scopes)
	if !ok {
		// Keep the scopes we understand from a hand-edited file.
		scopes = []string{ScopeModels}
		for _, scope := range g.Scopes {
			if scope == ScopeWeb {
				scopes = append(scopes, ScopeWeb)
				break
			}
		}
	}
	return scopes
}

func unionScopes(a, b []string) []string {
	out := make([]string, 0, len(AllScopes))
	for _, scope := range AllScopes {
		if slices.Contains(a, scope) || slices.Contains(b, scope) {
			out = append(out, scope)
		}
	}
	return out
}

func accessRefusal(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": message, "code": code})
}

// requestOrigin names where the request came from. Browsers always send
// Origin on cross-origin requests and pages cannot forge it.
func requestOrigin(c *gin.Context) string {
	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin == "" || origin == "null" {
		return "local program"
	}
	if len(origin) > 200 {
		origin = origin[:200]
	}
	return origin
}

// CreateAccessRequest handles POST /access/requests (no API key needed).
func (h *Handler) CreateAccessRequest(c *gin.Context) {
	if !isLoopbackPeer(c) {
		accessRefusal(c, http.StatusForbidden, "not_loopback", "access requests are only accepted from this computer")
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Key    string   `json:"key"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4096)).Decode(&body); err != nil {
		accessRefusal(c, http.StatusBadRequest, "invalid_request", "body must be JSON: {\"name\": string, \"key\": string, \"scopes\": [string]}")
		return
	}
	name := cleanName(body.Name)
	if name == "" {
		accessRefusal(c, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	if !validAccessKey(body.Key) {
		accessRefusal(c, http.StatusBadRequest, "invalid_request", "key must be 32-256 characters of A-Z a-z 0-9 - _ .")
		return
	}
	scopes, unknown, ok := normalizeScopes(body.Scopes)
	if !ok {
		accessRefusal(c, http.StatusBadRequest, "unknown_scope", "unknown scope: "+unknown)
		return
	}

	origin := requestOrigin(c)
	b := h.access()
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()

	// Lock order is always b.mu before h.mu.
	h.mu.Lock()
	known := h.cfg != nil && slices.Contains(h.cfg.APIKeys, body.Key)
	h.mu.Unlock()

	var upgradeOf *AccessGrant
	var added []string
	if known {
		b.loadGrantsLocked(h.grantsPath())
		hash := hashKey(body.Key)
		index := slices.IndexFunc(b.grants, func(g AccessGrant) bool { return g.KeyHash == hash })
		if index < 0 {
			accessRefusal(c, http.StatusConflict, "key_already_allowed", "this key is already allowed")
			return
		}
		grant := b.grants[index]
		if grant.Origin != origin {
			accessRefusal(c, http.StatusConflict, "key_owned_by_other_app", "this key belongs to another app")
			return
		}
		have := grantScopes(grant)
		for _, scope := range scopes {
			if !slices.Contains(have, scope) {
				added = append(added, scope)
			}
		}
		if len(added) == 0 {
			accessRefusal(c, http.StatusConflict, "scopes_already_granted", "this key already has these scopes")
			return
		}
		upgradeOf = &grant
		scopes = unionScopes(have, scopes)
	}

	b.expireLocked(now)
	if now.Sub(b.lastWatch) > accessWatcherWindow {
		accessRefusal(c, http.StatusServiceUnavailable, "no_approver", "no app is available to approve access requests")
		return
	}
	if until, ok := b.cooldown[origin]; ok && now.Before(until) {
		c.Header("Retry-After", strconv.Itoa(int(until.Sub(now).Seconds())+1))
		accessRefusal(c, http.StatusTooManyRequests, "cooldown", "access was just denied for this origin; try again later")
		return
	}
	pending := b.pendingLocked()
	for _, req := range pending {
		if req.Origin == origin {
			c.JSON(http.StatusConflict, gin.H{"error": "a request from this origin is already waiting", "code": "request_pending", "id": req.ID})
			return
		}
	}
	if len(pending) >= accessMaxPending {
		accessRefusal(c, http.StatusTooManyRequests, "too_many_pending", "too many pending access requests")
		return
	}

	ua := c.GetHeader("User-Agent")
	if len(ua) > 300 {
		ua = ua[:300]
	}
	req := &accessRequest{
		ID:        randomID(),
		Name:      name,
		Origin:    origin,
		UserAgent: ua,
		Status:    AccessPending,
		CreatedAt: now,
		ExpiresAt: now.Add(accessRequestTTL),
		Scopes:    scopes,
		key:       body.Key,
	}
	if upgradeOf != nil {
		req.Upgrade = true
		req.GrantID = upgradeOf.ID
		req.AddedScopes = added
	}
	b.requests[req.ID] = req
	b.notifyLocked()
	if req.Upgrade {
		log.Infof("access request %s from %s (%q) asks for more scopes %v and is waiting for approval", req.ID, origin, name, added)
	} else {
		log.Infof("access request %s from %s (%q) is waiting for approval", req.ID, origin, name)
	}
	c.JSON(http.StatusAccepted, gin.H{"id": req.ID, "status": req.Status, "expires_at": req.ExpiresAt, "scopes": req.Scopes})
}

// GetAccessRequest handles GET /access/requests/:id[?wait=seconds]. It reports
// only the status; the key never leaves the requester.
func (h *Handler) GetAccessRequest(c *gin.Context) {
	if !isLoopbackPeer(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "access requests are only accepted from this computer"})
		return
	}
	id := c.Param("id")
	b := h.access()
	deadline := time.Now().Add(waitDuration(c))
	for {
		b.mu.Lock()
		b.expireLocked(time.Now())
		req, ok := b.requests[id]
		var status string
		var scopes []string
		if ok {
			status = req.Status
			scopes = slices.Clone(req.Scopes)
		}
		changed := b.changed
		b.mu.Unlock()

		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown access request"})
			return
		}
		if status != AccessPending || !waitForChange(c.Request.Context(), changed, deadline) {
			out := gin.H{"id": id, "status": status}
			if status == AccessApproved {
				out["scopes"] = scopes
			}
			c.JSON(http.StatusOK, out)
			return
		}
	}
}

// waitForChange blocks until changed fires (true) or the deadline or request
// context ends (false).
func waitForChange(ctx context.Context, changed <-chan struct{}, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	// Wake up at least every 15s to expire requests even when nothing changes.
	if remaining > 15*time.Second {
		remaining = 15 * time.Second
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-changed:
		return true
	case <-timer.C:
		return time.Now().Before(deadline)
	case <-ctx.Done():
		return false
	}
}

// ListAccessRequests handles GET /v8/management/access/requests[?wait=seconds].
// With wait it returns as soon as at least one request is pending. Polling this
// endpoint is what enables POST /access/requests.
func (h *Handler) ListAccessRequests(c *gin.Context) {
	b := h.access()
	deadline := time.Now().Add(waitDuration(c))
	for {
		now := time.Now()
		b.mu.Lock()
		b.lastWatch = now
		b.expireLocked(now)
		pending := b.pendingLocked()
		changed := b.changed
		b.mu.Unlock()

		if len(pending) > 0 || !waitForChange(c.Request.Context(), changed, deadline) {
			c.JSON(http.StatusOK, gin.H{"requests": pending})
			return
		}
	}
}

// ApproveAccessRequest handles POST /v8/management/access/requests/:id/approve.
// b.mu is held throughout so a request can only be approved once; lock order
// is always b.mu before h.mu.
func (h *Handler) ApproveAccessRequest(c *gin.Context) {
	b := h.access()
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.expireLocked(now)
	req, ok := b.requests[c.Param("id")]
	if !ok || req.Status != AccessPending {
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending access request with this id"})
		return
	}
	path := h.grantsPath()
	b.loadGrantsLocked(path)

	if req.Upgrade {
		index := slices.IndexFunc(b.grants, func(g AccessGrant) bool { return g.ID == req.GrantID })
		h.mu.Lock()
		stillAllowed := h.cfg != nil && slices.Contains(h.cfg.APIKeys, req.key)
		h.mu.Unlock()
		if index < 0 || !stillAllowed {
			// The app was disconnected while the prompt was open.
			req.Status = AccessExpired
			req.resolvedAt = now
			req.key = ""
			b.notifyLocked()
			c.JSON(http.StatusConflict, gin.H{"error": "the app was disconnected; its request can no longer be approved", "code": "grant_revoked"})
			return
		}
		b.grants[index].Scopes = unionScopes(grantScopes(b.grants[index]), req.Scopes)
		if err := b.saveGrantsLocked(path); err != nil {
			log.Warnf("access grants: %v", err)
		}
		grant := b.grants[index]
		req.Scopes = slices.Clone(grant.Scopes)
		req.Status = AccessApproved
		req.resolvedAt = now
		req.key = ""
		b.notifyLocked()
		log.Infof("access request %s from %s approved: grant %s now has scopes %v", req.ID, req.Origin, grant.ID, grant.Scopes)
		c.JSON(http.StatusOK, grant)
		return
	}

	h.mu.Lock()
	if !slices.Contains(h.cfg.APIKeys, req.key) {
		h.cfg.APIKeys = append(h.cfg.APIKeys, req.key)
	}
	snapshot, saved := h.saveConfigAndSnapshotLocked(c)
	h.mu.Unlock()
	if !saved {
		return // response already written
	}
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)

	grant := AccessGrant{
		ID:        randomID(),
		Name:      req.Name,
		Origin:    req.Origin,
		KeyHash:   hashKey(req.key),
		KeyHint:   keyHint(req.key),
		GrantedAt: now,
		Scopes:    slices.Clone(req.Scopes),
	}
	// A stale grant for the same key (its key was removed some other way)
	// would otherwise shadow the new one.
	b.grants = slices.DeleteFunc(b.grants, func(g AccessGrant) bool { return g.KeyHash == grant.KeyHash })
	b.grants = append(b.grants, grant)
	if err := b.saveGrantsLocked(path); err != nil {
		log.Warnf("access grants: %v", err)
	}
	req.Status = AccessApproved
	req.resolvedAt = now
	req.key = ""
	b.notifyLocked()
	log.Infof("access request %s from %s approved with scopes %v", req.ID, req.Origin, req.Scopes)
	c.JSON(http.StatusOK, grant)
}

// DenyAccessRequest handles POST /v8/management/access/requests/:id/deny.
func (h *Handler) DenyAccessRequest(c *gin.Context) {
	b := h.access()
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked(now)
	req, ok := b.requests[c.Param("id")]
	if !ok || req.Status != AccessPending {
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending access request with this id"})
		return
	}
	req.Status = AccessDenied
	req.resolvedAt = now
	req.key = ""
	b.cooldown[req.Origin] = now.Add(accessDenyCooldown)
	b.notifyLocked()
	c.JSON(http.StatusOK, gin.H{"id": req.ID, "status": req.Status})
}

// ListAccessGrants handles GET /v8/management/access/grants. Grants whose key
// was removed from access.api-keys some other way are dropped.
func (h *Handler) ListAccessGrants(c *gin.Context) {
	h.mu.Lock()
	active := map[string]bool{}
	for _, key := range h.cfg.APIKeys {
		active[hashKey(key)] = true
	}
	h.mu.Unlock()

	b := h.access()
	b.mu.Lock()
	defer b.mu.Unlock()
	path := h.grantsPath()
	b.loadGrantsLocked(path)
	kept := b.grants[:0]
	for _, g := range b.grants {
		if active[g.KeyHash] {
			kept = append(kept, g)
		}
	}
	if len(kept) != len(b.grants) {
		b.grants = kept
		if err := b.saveGrantsLocked(path); err != nil {
			log.Warnf("access grants: %v", err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"grants": append([]AccessGrant{}, b.grants...)})
}

// RevokeAccessGrant handles DELETE /v8/management/access/grants/:id: it removes
// the app's key from access.api-keys and forgets the grant.
func (h *Handler) RevokeAccessGrant(c *gin.Context) {
	b := h.access()
	b.mu.Lock()
	defer b.mu.Unlock()
	path := h.grantsPath()
	b.loadGrantsLocked(path)
	index := slices.IndexFunc(b.grants, func(g AccessGrant) bool { return g.ID == c.Param("id") })
	if index < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown grant"})
		return
	}
	grant := b.grants[index]

	h.mu.Lock()
	h.cfg.APIKeys = slices.DeleteFunc(h.cfg.APIKeys, func(key string) bool { return hashKey(key) == grant.KeyHash })
	snapshot, saved := h.saveConfigAndSnapshotLocked(c)
	h.mu.Unlock()
	if !saved {
		return
	}
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)

	b.grants = slices.Delete(b.grants, index, index+1)
	if err := b.saveGrantsLocked(path); err != nil {
		log.Warnf("access grants: %v", err)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) grantsPath() string {
	if h.configFilePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.configFilePath), accessGrantsFileName)
}

func (b *accessBroker) loadGrantsLocked(path string) {
	if b.grantsLoaded {
		return
	}
	b.grantsLoaded = true
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("access grants: %v", err)
		}
		return
	}
	var file struct {
		Grants []AccessGrant `json:"grants"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		log.Warnf("access grants: %s is not valid JSON: %v", path, err)
		return
	}
	for i := range file.Grants {
		file.Grants[i].Scopes = grantScopes(file.Grants[i])
	}
	b.grants = file.Grants
}

// KeyScopes reports the scopes of an authenticated API key: the grant's
// scopes for a key approved through an access request, every scope for the
// user's own keys (no grant).
func (h *Handler) KeyScopes(key string) []string {
	if h == nil || key == "" {
		return slices.Clone(AllScopes)
	}
	hash := hashKey(key)
	b := h.access()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loadGrantsLocked(h.grantsPath())
	for _, g := range b.grants {
		if g.KeyHash == hash {
			return slices.Clone(grantScopes(g))
		}
	}
	return slices.Clone(AllScopes)
}

func (b *accessBroker) saveGrantsLocked(path string) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(map[string]any{"grants": b.grants}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
