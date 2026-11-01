package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type credential struct {
	Type      string `json:"type"`
	Access    string `json:"access,omitempty"`
	Refresh   string `json:"refresh,omitempty"`
	Expires   int64  `json:"expires,omitempty"`
	Key       string `json:"key,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	Plan      string `json:"plan,omitempty"`
}
type keyCache struct {
	Token   string
	Expires int64
}
type loginFlow struct {
	mu                   sync.Mutex
	verifier, uuid, mode string
	expires              time.Time
	result               any
}

func storedCredential(req rpcRequest) (credential, map[string]any, error) {
	raw := req.StorageJSON
	var stored map[string]any
	var err error
	if len(raw) > 0 {
		stored, err = jsonObject(raw)
		if err != nil {
			return credential{}, nil, err
		}
	} else {
		stored = req.AuthMetadata
		if stored == nil {
			stored = req.Metadata
		}
	}
	var auth credential
	if value := object(stored["cursor_auth"]); value != nil {
		_ = json.Unmarshal(encodeJSON(value), &auth)
	}
	if auth.Type != "oauth" && auth.Type != "api" {
		return auth, stored, failure(401, "Missing Cursor credentials")
	}
	if auth.Type == "api" && auth.Key == "" || auth.Type == "oauth" && auth.Access == "" && auth.Refresh != "cursor-agent" {
		return auth, stored, failure(401, "Missing Cursor access token or API key")
	}
	return auth, stored, nil
}
func checkProxy(req rpcRequest, stored map[string]any) error {
	if str(stored["proxy_url"]) != "" || req.AuthAttributes["proxy_url"] != "" || req.Attributes["proxy_url"] != "" || str(req.AuthMetadata["proxy_url"]) != "" || str(req.Metadata["proxy_url"]) != "" {
		return failure(501, "This CPA HTTP callback interface does not forward per-account proxies; configure requests.proxy-url globally and remove account proxy_url")
	}
	return nil
}
func authRecord(auth credential, name, label string, extra map[string]any) map[string]any {
	identity := auth.AccountID
	if identity == "" {
		secret := auth.Key
		if secret == "" {
			secret = auth.Access
		}
		if secret == "" {
			secret = auth.Refresh
		}
		identity = digest(secret)[:16]
	}
	stored := make(map[string]any, len(extra)+2)
	for k, v := range extra {
		stored[k] = v
	}
	stored["type"] = "cursor"
	stored["cursor_auth"] = auth
	if label != "" {
		stored["label"] = label
	}
	if name == "" {
		name = "cursor-" + digest(identity)[:16] + ".json"
	}
	if label == "" {
		label = auth.AccountID
	}
	if label == "" {
		label = "Cursor"
	}
	record := map[string]any{"Provider": "cursor", "ID": "cursor-" + identity, "FileName": name, "Label": label, "StorageJSON": encodeJSON(stored), "Metadata": stored}
	if renewable(auth) {
		// CPA's refresh loop calls auth.refresh only for credentials that name an interval;
		// refreshAuth itself contacts Cursor only within refreshLead of the token's expiry.
		record["Attributes"] = map[string]string{"refresh_interval_seconds": "3600"}
	}
	return record
}
func (p *pluginRuntime) parseAuth(req rpcRequest) (any, error) {
	stored, err := jsonObject(req.RawJSON)
	if err != nil {
		return nil, err
	}
	if str(stored["type"]) != "cursor" {
		return map[string]any{"Handled": false}, nil
	}
	auth, _, err := storedCredential(rpcRequest{StorageJSON: req.RawJSON})
	if err != nil {
		return nil, err
	}
	record := authRecord(auth, req.FileName, str(stored["label"]), stored)
	record["Prefix"] = str(stored["prefix"])
	record["ProxyURL"] = str(stored["proxy_url"])
	record["Disabled"] = stored["disabled"] == true
	return map[string]any{"Handled": true, "Auth": record}, nil
}
func (p *pluginRuntime) token(s *requestScope, req rpcRequest) (string, error) {
	auth, stored, err := storedCredential(req)
	if err != nil {
		return "", err
	}
	if err = checkProxy(req, stored); err != nil {
		return "", err
	}
	if auth.Type == "api" {
		key := digest(auth.Key)
		p.mu.Lock()
		had, ok := p.keys[key]
		p.mu.Unlock()
		now := p.now().UnixMilli()
		if ok && (had.Expires == 0 || had.Expires-now > 5*60*1000) {
			return had.Token, nil
		}
		result, err := s.json(s.ctx, http.MethodPost, "/auth/exchange_user_api_key", auth.Key, map[string]any{})
		if err != nil {
			return "", err
		}
		token := str(result["accessToken"])
		if token == "" {
			return "", failure(401, "Cursor did not accept this API key")
		}
		p.mu.Lock()
		if len(p.keys) >= 256 {
			p.keys = make(map[string]keyCache)
		}
		p.keys[key] = keyCache{token, jwtExpiry(token)}
		p.mu.Unlock()
		return token, nil
	}
	if auth.Refresh == "cursor-agent" {
		auth.Access, err = readCLIToken()
		if err != nil {
			return "", err
		}
		auth.Expires = 0
	}
	expires := auth.Expires
	if expires == 0 {
		expires = jwtExpiry(auth.Access)
	}
	if auth.Access == "" {
		return "", failure(401, "Cursor is not signed in; use CPA's Cursor browser login")
	}
	if expires > 0 && expires <= p.now().UnixMilli() {
		return "", failure(401, "Cursor token expired; sign in to Cursor again")
	}
	return auth.Access, nil
}

// cursorClientID is the OAuth client Cursor's IDE and CLI renew their login sessions with.
const cursorClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"

// refreshLead renews a browser session this long before its access token expires.
const refreshLead = 24 * time.Hour

// renewable reports whether a credential holds a refresh token issued by browser login.
func renewable(auth credential) bool {
	return auth.Type == "oauth" && auth.Refresh != "" && auth.Refresh != "none" && auth.Refresh != "cursor-agent"
}
func (p *pluginRuntime) refreshAuth(s *requestScope, req rpcRequest) (any, error) {
	auth, stored, err := storedCredential(req)
	if err != nil {
		return nil, err
	}
	now := p.now()
	expires := auth.Expires
	if expires == 0 {
		expires = jwtExpiry(auth.Access)
	}
	if renewable(auth) && (auth.Access == "" || expires > 0 && expires-now.UnixMilli() <= refreshLead.Milliseconds()) {
		if err = checkProxy(req, stored); err != nil {
			return nil, err
		}
		if auth, err = p.renew(s, auth); err != nil {
			return nil, err
		}
		expires = auth.Expires
	} else if auth.Type == "oauth" {
		if _, err = p.token(s, req); err != nil {
			return nil, err
		}
	}
	record := authRecord(auth, req.FileName, str(stored["label"]), stored)
	if req.AuthID != "" {
		record["ID"] = req.AuthID
	}
	next := now.Add(time.Hour)
	if renewable(auth) && expires > 0 {
		if due := time.UnixMilli(expires).Add(-refreshLead); due.After(now) && due.Before(next) {
			next = due
		}
	}
	return map[string]any{"Auth": record, "NextRefreshAfter": next.UTC().Format(time.RFC3339)}, nil
}

// renew exchanges the stored refresh token for a new access token. Cursor answers a
// session it will not renew with HTTP 200, an empty token and shouldLogout.
//
// Browser login stores one session token as both access and refresh token, and renewal
// returns a new session token without a refresh token. When the stored refresh token is
// a JWT that expires before the new token, the new token becomes the refresh token too,
// so renewal does not depend on the original login's expiry.
func (p *pluginRuntime) renew(s *requestScope, auth credential) (credential, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	result, err := s.json(ctx, http.MethodPost, "/oauth/token", "", map[string]any{"grant_type": "refresh_token", "client_id": cursorClientID, "refresh_token": auth.Refresh})
	if err != nil {
		return auth, err
	}
	token := str(result["access_token"])
	if result["shouldLogout"] == true || token == "" {
		return auth, failure(401, "Cursor ended this session; sign in to Cursor again")
	}
	auth.Access = token
	auth.Expires = jwtExpiry(token)
	if refresh := str(result["refresh_token"]); refresh != "" {
		auth.Refresh = refresh
	} else if expires := jwtExpiry(auth.Refresh); expires > 0 && auth.Expires > expires {
		auth.Refresh = token
	}
	return auth, nil
}
func (p *pluginRuntime) startLogin(s *requestScope, req rpcRequest) (any, error) {
	now := p.now()
	p.mu.Lock()
	for id, flow := range p.logins {
		if !now.Before(flow.expires) {
			delete(p.logins, id)
		}
	}
	if len(p.logins) >= 128 {
		p.mu.Unlock()
		return nil, failure(429, "Too many pending Cursor logins")
	}
	p.mu.Unlock()
	mode := str(req.Metadata["mode"])
	if mode == "" {
		mode = "browser"
		if strings.TrimSpace(str(req.Metadata["api_key"])) != "" {
			mode = "api"
		}
	}
	if mode != "browser" && mode != "cli" && mode != "api" {
		return nil, failure(400, "Cursor login mode must be browser, cli or api")
	}
	var verifierBytes [32]byte
	if _, err := rand.Read(verifierBytes[:]); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes[:])
	challenge := sha256.Sum256([]byte(verifier))
	state, uuid := randomID(), randomID()
	flow := &loginFlow{verifier: verifier, uuid: uuid, mode: mode, expires: now.Add(15 * time.Minute)}
	loginURL := ""
	switch mode {
	case "browser":
		u, _ := url.Parse("https://cursor.com/loginDeepControl")
		q := u.Query()
		q.Set("challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
		q.Set("uuid", uuid)
		q.Set("mode", "login")
		q.Set("redirectTarget", "cli")
		u.RawQuery = q.Encode()
		loginURL = u.String()
	case "cli":
		token, err := readCLIToken()
		if err != nil {
			return nil, err
		}
		if token == "" {
			return nil, failure(401, "cursor-agent is not signed in; use browser login")
		}
		flow.result = map[string]any{"Status": "success", "Auth": authRecord(credential{Type: "oauth", Refresh: "cursor-agent"}, "", "Cursor CLI", nil)}
	case "api":
		key := strings.TrimSpace(str(req.Metadata["api_key"]))
		if key == "" || strings.ContainsAny(key, "\r\n\t ") {
			return nil, failure(400, "Provide Metadata.api_key for API key import")
		}
		flow.result = map[string]any{"Status": "success", "Auth": authRecord(credential{Type: "api", Key: key}, "", "Cursor API key", nil)}
	}
	p.mu.Lock()
	p.logins[state] = flow
	p.mu.Unlock()
	return map[string]any{"Provider": "cursor", "URL": loginURL, "State": state, "ExpiresAt": flow.expires.UTC().Format(time.RFC3339)}, nil
}
func (p *pluginRuntime) pollLogin(s *requestScope, req rpcRequest) (any, error) {
	p.mu.Lock()
	flow := p.logins[req.State]
	p.mu.Unlock()
	if flow == nil || !p.now().Before(flow.expires) {
		return map[string]any{"Status": "error", "Message": "Login expired; start again"}, nil
	}
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.result != nil {
		return flow.result, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	path := "/auth/poll?" + url.Values{"uuid": {flow.uuid}, "verifier": {flow.verifier}}.Encode()
	result, err := s.json(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		if errorStatus(err) == 404 {
			return map[string]any{"Status": "pending"}, nil
		}
		return nil, err
	}
	token := str(result["accessToken"])
	if token == "" {
		return nil, failure(502, "Cursor returned no access token")
	}
	expires := jwtExpiry(token)
	if expires == 0 {
		expires = p.now().Add(30 * 24 * time.Hour).UnixMilli()
	}
	refresh := str(result["refreshToken"])
	if refresh == "" {
		refresh = "none"
	}
	auth := credential{Type: "oauth", Access: token, Refresh: refresh, Expires: expires}
	if me, err := s.json(ctx, http.MethodPost, "/aiserver.v1.DashboardService/GetMe", token, map[string]any{}); err == nil {
		auth.AccountID = str(me["email"])
	}
	flow.result = map[string]any{"Status": "success", "Auth": authRecord(auth, "", "", nil)}
	return flow.result, nil
}
func readCLIToken() (string, error) {
	if runtime.GOOS == "darwin" {
		if token, err := readKeychainToken(); err == nil && token != "" {
			return token, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var path string
	switch runtime.GOOS {
	case "darwin":
		path = filepath.Join(home, ".cursor", "auth.json")
	case "windows":
		dir := os.Getenv("APPDATA")
		if dir == "" {
			dir = filepath.Join(home, "AppData", "Roaming")
		}
		path = filepath.Join(dir, "Cursor", "auth.json")
	default:
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		path = filepath.Join(dir, "cursor", "auth.json")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var stored struct {
		AccessToken string `json:"accessToken"`
	}
	if err = json.Unmarshal(data, &stored); err != nil {
		return "", failure(401, "Invalid cursor-agent auth file")
	}
	if exp := jwtExpiry(stored.AccessToken); exp > 0 && exp <= time.Now().UnixMilli() {
		return "", failure(401, "cursor-agent token expired; sign in again")
	}
	return strings.TrimSpace(stored.AccessToken), nil
}
