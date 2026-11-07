package main

import (
	"bytes"
	"testing"
)

func TestClientIdentitySplit(t *testing.T) {
	if got := headers("").Get("X-Cursor-Client-Type"); got != "cli" {
		t.Fatalf("agent header client type = %q", got)
	}
	if agentClientType != "cli" {
		t.Fatalf("agent client type = %q", agentClientType)
	}
	if got := headersWithType("token", catalogClientType).Get("X-Cursor-Client-Type"); got != "desktop" {
		t.Fatalf("catalog header client type = %q", got)
	}
	if headersWithType("token", catalogClientType).Get("Authorization") != "Bearer token" {
		t.Fatal("catalog header dropped the bearer token")
	}
	run, _ := buildRun(chatConversation{Last: "ping"}, nil, modelResolution{ID: "grok"}, "conv")
	if bytes.Contains(run, []byte("desktop")) {
		t.Fatal("agent run payload advertises desktop")
	}
	if !bytes.Contains(run, []byte(agentClientType)) {
		t.Fatal("agent run payload is missing the cli identity")
	}
}
