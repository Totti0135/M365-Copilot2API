package web

import (
	"m365-copilot2api/internal/chathub"
	"testing"
)

func TestContentPolicyBlock(t *testing.T) {
	if !isContentPolicyBlock("很抱歉，我无法响应。我可以提供其他方面的帮助吗？") {
		t.Fatal("M365 refusal was not detected")
	}
	if !isContentPolicyBlock("很抱歉，我无法提供此类内容") {
		t.Fatal("broader Chinese refusal was not detected")
	}
	if isContentPolicyBlock("OK") {
		t.Fatal("ordinary response was classified as a refusal")
	}
	if isContentPolicyBlock("I'm sorry, I can't help with that") {
		t.Fatal("normal model refusal was misclassified as content policy block")
	}
	if !chathub.IsContentPolicyBlock("很抱歉，我无法生成该图像") {
		t.Fatal("chathub.IsContentPolicyBlock did not detect refusal")
	}
}
