package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixtureJWT(exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString(encodeJSON(map[string]any{"exp": exp.Unix(), "sub": "github|1"}))
	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
}
func refreshedCredential(t *testing.T, result any) credential {
	t.Helper()
	record := object(result.(map[string]any)["Auth"])
	stored, err := jsonObject(record["StorageJSON"].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	var auth credential
	if err = json.Unmarshal(encodeJSON(stored["cursor_auth"]), &auth); err != nil {
		t.Fatal(err)
	}
	return auth
}
func TestNativeRefreshRenewsBrowserSessionBeforeExpiry(t *testing.T) {
	now := time.Unix(1800000000, 0)
	renewed := fixtureJWT(now.Add(60 * 24 * time.Hour))
	session := fixtureJWT(now.Add(time.Hour))
	longLived := fixtureJWT(now.Add(90 * 24 * time.Hour))
	for _, tc := range []struct {
		name, refresh, rotated, want string
	}{
		{name: "opaque refresh token kept", refresh: "fixture-refresh", want: "fixture-refresh"},
		{name: "rotated refresh token stored", refresh: "fixture-refresh", rotated: "rotated-refresh", want: "rotated-refresh"},
		{name: "session token chained", refresh: session, want: renewed},
		{name: "longer-lived refresh JWT kept", refresh: longLived, want: longLived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestRuntime(t)
			p.now = func() time.Time { return now }
			h := newMockHost(t)
			s := p.newScope(h.call, "refresh")
			calls := 0
			h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
				calls++
				if req.Method != "POST" || !strings.HasSuffix(req.URL, "/oauth/token") {
					t.Errorf("unexpected request %s %s", req.Method, req.URL)
				}
				if req.Headers.Get("Connect-Protocol-Version") != "" || req.Headers.Get("Authorization") != "" {
					t.Errorf("refresh sent RPC headers: %v", req.Headers)
				}
				body, _ := jsonObject(req.Body)
				if body["grant_type"] != "refresh_token" || body["client_id"] != cursorClientID || body["refresh_token"] != tc.refresh {
					t.Errorf("unexpected refresh body %v", body)
				}
				reply := map[string]any{"access_token": renewed}
				if tc.rotated != "" {
					reply["refresh_token"] = tc.rotated
				}
				return mockResponse{Status: 200, Body: encodeJSON(reply)}, nil
			}
			stored := map[string]any{"type": "cursor", "label": "Personal", "cursor_auth": map[string]any{"type": "oauth", "access": session, "refresh": tc.refresh, "expires": now.Add(time.Hour).UnixMilli(), "accountId": "fixture@example.invalid"}}
			result, err := p.refreshAuth(s, rpcRequest{StorageJSON: encodeJSON(stored), AuthID: "cursor-fixture", FileName: "cursor-fixture.json"})
			s.close()
			if err != nil {
				t.Fatal(err)
			}
			auth := refreshedCredential(t, result)
			if calls != 1 || auth.Access != renewed || auth.Refresh != tc.want || auth.Expires != now.Add(60*24*time.Hour).UnixMilli() || auth.AccountID != "fixture@example.invalid" {
				t.Fatalf("renewal not persisted: calls=%d auth=%+v", calls, auth)
			}
			out := result.(map[string]any)
			record := object(out["Auth"])
			if record["ID"] != "cursor-fixture" || out["NextRefreshAfter"] != now.Add(time.Hour).UTC().Format(time.RFC3339) {
				t.Fatalf("unexpected refresh record %v", out)
			}
			if attrs, _ := record["Attributes"].(map[string]string); attrs["refresh_interval_seconds"] != "3600" {
				t.Fatalf("renewable login not scheduled for host refresh: %v", record["Attributes"])
			}
		})
	}
}
func TestNativeAuthRecordSchedulesOnlyRenewableLogins(t *testing.T) {
	for _, tc := range []struct {
		auth credential
		want bool
	}{
		{credential{Type: "oauth", Access: "a", Refresh: "fixture-refresh"}, true},
		{credential{Type: "oauth", Access: "a", Refresh: "none"}, false},
		{credential{Type: "oauth", Refresh: "cursor-agent"}, false},
		{credential{Type: "api", Key: "k"}, false},
	} {
		_, scheduled := authRecord(tc.auth, "", "", nil)["Attributes"]
		if scheduled != tc.want {
			t.Fatalf("%+v: scheduled=%t, want %t", tc.auth, scheduled, tc.want)
		}
	}
}
func TestNativeRefreshSchedulesRenewalAndSkipsFreshTokens(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct {
		name    string
		expires time.Time
		refresh string
		next    time.Time
	}{
		{"fresh", now.Add(10 * 24 * time.Hour), "fixture-refresh", now.Add(time.Hour)},
		{"due within the hour", now.Add(refreshLead + 30*time.Minute), "fixture-refresh", now.Add(30 * time.Minute)},
		{"no refresh token", now.Add(time.Hour), "none", now.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestRuntime(t)
			p.now = func() time.Time { return now }
			h := newMockHost(t)
			s := p.newScope(h.call, "refresh")
			defer s.close()
			h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
				t.Errorf("unexpected request %s", req.URL)
				return mockResponse{Status: 500}, nil
			}
			access := fixtureJWT(tc.expires)
			stored := map[string]any{"type": "cursor", "cursor_auth": map[string]any{"type": "oauth", "access": access, "refresh": tc.refresh, "expires": tc.expires.UnixMilli()}}
			result, err := p.refreshAuth(s, rpcRequest{StorageJSON: encodeJSON(stored)})
			if err != nil {
				t.Fatal(err)
			}
			if auth := refreshedCredential(t, result); auth.Access != access {
				t.Fatal("fresh token was replaced")
			}
			if next := result.(map[string]any)["NextRefreshAfter"]; next != tc.next.UTC().Format(time.RFC3339) {
				t.Fatalf("NextRefreshAfter = %v, want %v", next, tc.next.UTC().Format(time.RFC3339))
			}
		})
	}
}
func TestNativeRefreshPreservesAccountSettings(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, disabled := range []bool{false, true} {
		for _, lifetime := range []time.Duration{time.Hour, 10 * 24 * time.Hour} {
			p := newTestRuntime(t)
			p.now = func() time.Time { return now }
			h := newMockHost(t)
			s := p.newScope(h.call, "refresh")
			defer s.close()
			h.respond = func(context.Context, mockRequest) (mockResponse, error) {
				return mockResponse{Status: 200, Body: encodeJSON(map[string]any{"access_token": fixtureJWT(now.Add(60 * 24 * time.Hour))})}, nil
			}
			stored := map[string]any{"type": "cursor", "disabled": disabled, "prefix": "work", "priority": 3,
				"cursor_auth": credential{Type: "oauth", Access: fixtureJWT(now.Add(lifetime)), Refresh: "fixture-refresh"}}
			// Like the real CPA callback, omit FileName and Label.
			result, err := p.refreshAuth(s, rpcRequest{StorageJSON: encodeJSON(stored), AuthID: "custom-account"})
			if err != nil {
				t.Fatal(err)
			}
			record := object(result.(map[string]any)["Auth"])
			if record["Disabled"] != disabled || record["ID"] != "custom-account" || record["FileName"] != "" || record["Label"] != "" {
				t.Fatalf("refresh changed host account settings: %v", record)
			}
			persisted, err := jsonObject(record["StorageJSON"].([]byte))
			if err != nil {
				t.Fatal(err)
			}
			delete(persisted, "cursor_auth")
			delete(stored, "cursor_auth")
			before, _ := jsonObject(encodeJSON(stored))
			if !reflect.DeepEqual(persisted, before) {
				t.Fatalf("account metadata changed: got %v want %v", persisted, before)
			}
		}
	}
}

func TestNativeInvalidRenewalKeepsCurrentTokenUntilExpiry(t *testing.T) {
	initial := time.Unix(1800000000, 0)
	for name, reply := range map[string]mockResponse{
		"shouldLogout":       {Status: 200, Body: []byte(`{"access_token":"","shouldLogout":true}`)},
		"missing token":      {Status: 200, Body: []byte(`{}`)},
		"malformed token":    {Status: 200, Body: []byte(`{"access_token":"malformed-token","refresh_token":"do-not-save"}`)},
		"missing expiry":     {Status: 200, Body: []byte(`{"access_token":"e30.e30.sig"}`)},
		"expired token":      {Status: 200, Body: encodeJSON(map[string]any{"access_token": fixtureJWT(initial.Add(-time.Minute))})},
		"expires now":        {Status: 200, Body: encodeJSON(map[string]any{"access_token": fixtureJWT(initial)})},
		"unauthorized grant": {Status: 401, Body: []byte(`{"error":"invalid_grant"}`)},
		"invalid grant":      {Status: 400, Body: []byte(`{"error":"invalid_grant"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			now := initial
			p := newTestRuntime(t)
			p.now = func() time.Time { return now }
			h := newMockHost(t)
			s := p.newScope(h.call, "refresh")
			defer s.close()
			calls := 0
			h.respond = func(context.Context, mockRequest) (mockResponse, error) {
				calls++
				return reply, nil
			}
			original := credential{Type: "oauth", Access: fixtureJWT(now.Add(2 * time.Hour)), Refresh: "fixture-refresh", Expires: now.Add(2 * time.Hour).UnixMilli()}
			stored := map[string]any{"type": "cursor", "disabled": true, "label": "Personal", "cursor_auth": original}
			result, err := p.refreshAuth(s, rpcRequest{StorageJSON: encodeJSON(stored)})
			if err != nil {
				t.Fatalf("invalid renewal must not fail the existing account: %v", err)
			}
			want := original
			want.RefreshStopped = true
			if got := refreshedCredential(t, result); got != want {
				t.Fatalf("existing credentials replaced: got %+v want %+v", got, want)
			}
			record := object(result.(map[string]any)["Auth"])
			if record["Disabled"] != true || record["Label"] != "Personal" {
				t.Fatalf("account state changed: %v", record)
			}
			attrs := record["Attributes"].(map[string]string)
			if attrs["refresh_interval_seconds"] != "0" {
				t.Fatalf("host refresh interval not cleared: %v", attrs)
			}
			raw := record["StorageJSON"].([]byte)
			// Restart and reload from persisted JSON; the stop must survive both.
			p2 := newTestRuntime(t)
			p2.now = func() time.Time { return now }
			s2 := p2.newScope(h.call, "reloaded")
			defer s2.close()
			parsed, err := p2.parseAuth(rpcRequest{RawJSON: raw})
			if err != nil {
				t.Fatal(err)
			}
			reloaded := object(parsed.(map[string]any)["Auth"])
			if reloaded["Attributes"].(map[string]string)["refresh_interval_seconds"] != "0" {
				t.Fatal("reload re-enabled renewal")
			}
			req := rpcRequest{StorageJSON: raw}
			now = now.Add(time.Hour)
			if token, err := p2.token(s2, req); err != nil || token != original.Access {
				t.Fatalf("current token unusable before expiry: %v", err)
			}
			if _, err := p2.refreshAuth(s2, req); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			if _, err := p2.token(s2, req); errorStatus(err) != 401 {
				t.Fatalf("expired current token was accepted: %v", err)
			}
			if _, err := p2.refreshAuth(s2, req); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("invalid renewal retried %d times", calls-1)
			}
			// A new login has no stop marker and can renew normally.
			fresh, err := p2.parseAuth(rpcRequest{RawJSON: encodeJSON(stored)})
			if err != nil || object(fresh.(map[string]any)["Auth"])["Attributes"].(map[string]string)["refresh_interval_seconds"] != "3600" {
				t.Fatalf("new login did not restore renewal: %v", err)
			}
		})
	}
}

func TestNativeTransientRenewalFailureDoesNotStopRefresh(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		p := newTestRuntime(t)
		h := newMockHost(t)
		s := p.newScope(h.call, "refresh")
		defer s.close()
		h.respond = func(context.Context, mockRequest) (mockResponse, error) {
			return mockResponse{Status: status, Body: []byte(`{"message":"temporary failure"}`)}, nil
		}
		original := credential{Type: "oauth", Access: fixtureJWT(p.now().Add(time.Hour)), Refresh: "fixture-refresh"}
		got, err := p.renew(s, original)
		if errorStatus(err) != status || got != original {
			t.Fatalf("transient failure changed credentials: auth=%+v error=%v", got, err)
		}
	}
}
