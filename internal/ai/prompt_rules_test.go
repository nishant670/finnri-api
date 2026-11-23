package ai

import (
	"strings"
	"testing"
)

// The parse prompt's split rule used to explain only the one-friend case, and
// with two friends the model halved the bill and left the user at ₹0
// (2026-11-23). The API also clears such shares; this keeps the rule itself.
func TestPromptCountsTheUserInAnEqualSplit(t *testing.T) {
	if !strings.Contains(promptText, "number of friends + 1") {
		t.Fatal("the split rule must tell the model to count the user in an equal split")
	}
}

// "Petrol is NOT the Transport category" was written for questions and read as
// a capture rule: "Petrol 1500 on Rewards Credit Card" was filed as Misc.
func TestPromptKeepsPetrolCapturesInTransport(t *testing.T) {
	if strings.Contains(promptText, "Petrol is NOT the Transport category") {
		t.Fatal("the question rule about petrol must not read as a capture rule")
	}
	if !strings.Contains(promptText, "a petrol purchase being captured is still filed under Transport") {
		t.Fatal("the prompt must say a captured petrol purchase is Transport")
	}
}
