package ai

import (
	"errors"
	"testing"
)

func TestDefaultActionRegistryIsValid(t *testing.T) {
	registry := DefaultActionRegistry()

	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultActionRegistryDefinesCurrentParseActions(t *testing.T) {
	registry := DefaultActionRegistry()

	text, err := registry.RequireImplemented(ActionTransactionParseText)
	if err != nil {
		t.Fatal(err)
	}
	if text.DefaultCredits != 5 {
		t.Fatalf("text default credits = %d", text.DefaultCredits)
	}
	if text.InputKind != InputKindText {
		t.Fatalf("text input kind = %q", text.InputKind)
	}
	if !text.GuestAllowed {
		t.Fatal("text parse should be allowed for guests")
	}
	if len(text.ProviderOperations) != 1 || text.ProviderOperations[0] != ProviderOperationLLM {
		t.Fatalf("text provider operations = %#v", text.ProviderOperations)
	}

	voice, err := registry.RequireImplemented(ActionTransactionParseVoiceShort)
	if err != nil {
		t.Fatal(err)
	}
	if voice.DefaultCredits != 12 {
		t.Fatalf("voice default credits = %d", voice.DefaultCredits)
	}
	if voice.InputLimits.MaxAudioSeconds != 30 {
		t.Fatalf("voice max audio seconds = %d", voice.InputLimits.MaxAudioSeconds)
	}
	if len(voice.ProviderOperations) != 2 ||
		voice.ProviderOperations[0] != ProviderOperationTranscription ||
		voice.ProviderOperations[1] != ProviderOperationLLM {
		t.Fatalf("voice provider operations = %#v", voice.ProviderOperations)
	}
}

func TestUnknownActionCannotBeRequired(t *testing.T) {
	registry := DefaultActionRegistry()

	_, err := registry.Require(ActionCode("made_up_action"))
	if !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("expected ErrUnknownAction, got %v", err)
	}
}

func TestFutureActionsCannotBeExecutedYet(t *testing.T) {
	registry := DefaultActionRegistry()

	if _, err := registry.Require(ActionFutureAIAdvisorMessage); err != nil {
		t.Fatalf("future advisor action should be registered: %v", err)
	}
	if _, err := registry.RequireImplemented(ActionFutureAIAdvisorMessage); err == nil {
		t.Fatal("future advisor action should not be executable yet")
	}
}

func TestStatementScreenshotActionIsExecutableAndBounded(t *testing.T) {
	action, err := DefaultActionRegistry().RequireImplemented(ActionFutureAIStatementImport)
	if err != nil {
		t.Fatal(err)
	}
	if action.InputKind != InputKindFile || action.InputLimits.MaxFileBytes != 5*1024*1024 {
		t.Fatalf("unexpected statement input limits: %#v", action)
	}
	if !action.PaidPlanRequired || action.GuestAllowed {
		t.Fatalf("statement image parsing must stay paid and signed-in only: %#v", action)
	}
}

func TestActionValidationRejectsInvalidCosts(t *testing.T) {
	action := Action{
		Code:               ActionCode("bad_cost"),
		Label:              "Bad cost",
		InputKind:          InputKindText,
		DefaultCredits:     10,
		MaxCredits:         5,
		ProviderOperations: []ProviderOperation{ProviderOperationLLM},
	}

	if err := action.Validate(); err == nil {
		t.Fatal("expected invalid cost error")
	}
}
