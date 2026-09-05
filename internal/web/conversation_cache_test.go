package web

import "testing"

func TestConversationCacheIsolatedByNamespace(t *testing.T) {
	c := newConversationCache()
	c.Store("tenant-a\x00session-a", "account", "model", &cachedConversation{ConversationID: "conversation-a"})
	c.Store("tenant-a\x00session-b", "account", "model", &cachedConversation{ConversationID: "conversation-b"})

	if got := c.Lookup("tenant-a\x00session-a", "account", "model"); got == nil || got.ConversationID != "conversation-a" {
		t.Fatalf("session-a lookup=%#v", got)
	}
	if got := c.Lookup("tenant-a\x00session-b", "account", "model"); got == nil || got.ConversationID != "conversation-b" {
		t.Fatalf("session-b lookup=%#v", got)
	}
	if got := c.Lookup("tenant-b\x00session-a", "account", "model"); got != nil {
		t.Fatalf("cross-tenant cache leak=%#v", got)
	}
}
