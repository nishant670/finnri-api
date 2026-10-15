package ai

import (
	"errors"
	"fmt"
)

type ActionCode string

const (
	ActionTransactionParseText        ActionCode = "transaction_parse_text"
	ActionTransactionParseVoiceShort  ActionCode = "transaction_parse_voice_short"
	ActionTransactionParseVoiceMedium ActionCode = "transaction_parse_voice_medium"
	ActionTransactionParseVoiceLong   ActionCode = "transaction_parse_voice_long"
	ActionTransactionParseReceipt     ActionCode = "transaction_parse_receipt"
	ActionFutureAIAdvisorMessage      ActionCode = "future_ai_advisor_message"
	ActionFutureAIWeeklySummary       ActionCode = "future_ai_weekly_summary"
	ActionFutureAIBulkCategorization  ActionCode = "future_ai_bulk_categorization"
	ActionFutureAIStatementImport     ActionCode = "future_ai_statement_import"
)

type InputKind string

const (
	InputKindText  InputKind = "text"
	InputKindVoice InputKind = "voice"
	InputKindImage InputKind = "image"
	InputKindFile  InputKind = "file"
	InputKindChat  InputKind = "chat"
)

type ProviderOperation string

const (
	ProviderOperationLLM           ProviderOperation = "llm"
	ProviderOperationTranscription ProviderOperation = "transcription"
)

var ErrUnknownAction = errors.New("unknown ai action")

type ActionInputLimits struct {
	MaxTranscriptChars int
	MaxAudioSeconds    int
	MaxBatchItems      int
	MaxFileBytes       int64
}

type Action struct {
	Code               ActionCode
	Label              string
	InputKind          InputKind
	GuestAllowed       bool
	DefaultCredits     int
	MaxCredits         int
	ProviderOperations []ProviderOperation
	InputLimits        ActionInputLimits
	PaidPlanRequired   bool
	Implemented        bool
}

type ActionRegistry struct {
	actions map[ActionCode]Action
}

func DefaultActionRegistry() ActionRegistry {
	return NewActionRegistry([]Action{
		{
			Code:               ActionTransactionParseText,
			Label:              "Text AI transaction parse",
			InputKind:          InputKindText,
			GuestAllowed:       true,
			DefaultCredits:     5,
			MaxCredits:         10,
			ProviderOperations: []ProviderOperation{ProviderOperationLLM},
			InputLimits: ActionInputLimits{
				MaxTranscriptChars: 1000,
			},
			Implemented: true,
		},
		{
			Code:               ActionTransactionParseVoiceShort,
			Label:              "Short voice AI transaction parse",
			InputKind:          InputKindVoice,
			GuestAllowed:       true,
			DefaultCredits:     12,
			MaxCredits:         18,
			ProviderOperations: []ProviderOperation{ProviderOperationTranscription, ProviderOperationLLM},
			InputLimits: ActionInputLimits{
				MaxTranscriptChars: 1000,
				MaxAudioSeconds:    15,
			},
			Implemented: true,
		},
		{
			Code:               ActionTransactionParseVoiceMedium,
			Label:              "Medium voice AI transaction parse",
			InputKind:          InputKindVoice,
			GuestAllowed:       false,
			DefaultCredits:     18,
			MaxCredits:         30,
			ProviderOperations: []ProviderOperation{ProviderOperationTranscription, ProviderOperationLLM},
			InputLimits: ActionInputLimits{
				MaxTranscriptChars: 1000,
				MaxAudioSeconds:    30,
			},
			Implemented: true,
		},
		{
			Code:               ActionTransactionParseVoiceLong,
			Label:              "Long voice AI transaction parse",
			InputKind:          InputKindVoice,
			GuestAllowed:       false,
			DefaultCredits:     30,
			MaxCredits:         60,
			ProviderOperations: []ProviderOperation{ProviderOperationTranscription, ProviderOperationLLM},
			InputLimits: ActionInputLimits{
				MaxTranscriptChars: 1000,
				MaxAudioSeconds:    60,
			},
			PaidPlanRequired: true,
			Implemented:      false,
		},
		{
			Code:               ActionTransactionParseReceipt,
			Label:              "Receipt photo AI transaction parse",
			InputKind:          InputKindImage,
			GuestAllowed:       true,
			DefaultCredits:     25,
			MaxCredits:         50,
			ProviderOperations: []ProviderOperation{ProviderOperationLLM},
			InputLimits: ActionInputLimits{
				MaxFileBytes: 5 * 1024 * 1024,
			},
			Implemented: true,
		},
		{
			Code:               ActionFutureAIAdvisorMessage,
			Label:              "AI advisor message",
			InputKind:          InputKindChat,
			GuestAllowed:       false,
			DefaultCredits:     35,
			MaxCredits:         80,
			ProviderOperations: []ProviderOperation{ProviderOperationLLM},
			PaidPlanRequired:   true,
			Implemented:        false,
		},
		{
			Code:               ActionFutureAIWeeklySummary,
			Label:              "AI weekly summary",
			InputKind:          InputKindText,
			GuestAllowed:       false,
			DefaultCredits:     25,
			MaxCredits:         60,
			ProviderOperations: []ProviderOperation{ProviderOperationLLM},
			PaidPlanRequired:   true,
			Implemented:        false,
		},
		{
			Code:               ActionFutureAIBulkCategorization,
			Label:              "AI bulk categorization",
			InputKind:          InputKindText,
			GuestAllowed:       false,
			DefaultCredits:     50,
			MaxCredits:         150,
			ProviderOperations: []ProviderOperation{ProviderOperationLLM},
			InputLimits: ActionInputLimits{
				MaxBatchItems: 50,
			},
			PaidPlanRequired: true,
			Implemented:      false,
		},
		{
			Code:               ActionFutureAIStatementImport,
			Label:              "AI statement screenshot import",
			InputKind:          InputKindFile,
			GuestAllowed:       false,
			DefaultCredits:     100,
			MaxCredits:         300,
			ProviderOperations: []ProviderOperation{ProviderOperationLLM},
			InputLimits: ActionInputLimits{
				MaxFileBytes: 5 * 1024 * 1024,
			},
			PaidPlanRequired: true,
			Implemented:      true,
		},
	})
}

func NewActionRegistry(actions []Action) ActionRegistry {
	registry := ActionRegistry{actions: make(map[ActionCode]Action, len(actions))}
	for _, action := range actions {
		registry.actions[action.Code] = action
	}
	return registry
}

func (r ActionRegistry) Lookup(code ActionCode) (Action, bool) {
	action, ok := r.actions[code]
	return action, ok
}

func (r ActionRegistry) Require(code ActionCode) (Action, error) {
	action, ok := r.Lookup(code)
	if !ok {
		return Action{}, fmt.Errorf("%w: %s", ErrUnknownAction, code)
	}
	return action, nil
}

func (r ActionRegistry) RequireImplemented(code ActionCode) (Action, error) {
	action, err := r.Require(code)
	if err != nil {
		return Action{}, err
	}
	if !action.Implemented {
		return Action{}, fmt.Errorf("ai action not implemented: %s", code)
	}
	return action, nil
}

func (a Action) Validate() error {
	if a.Code == "" {
		return errors.New("action code is required")
	}
	if a.Label == "" {
		return fmt.Errorf("action %s label is required", a.Code)
	}
	if a.InputKind == "" {
		return fmt.Errorf("action %s input kind is required", a.Code)
	}
	if a.DefaultCredits <= 0 {
		return fmt.Errorf("action %s default credits must be positive", a.Code)
	}
	if a.MaxCredits < a.DefaultCredits {
		return fmt.Errorf("action %s max credits must be greater than or equal to default credits", a.Code)
	}
	if len(a.ProviderOperations) == 0 {
		return fmt.Errorf("action %s must declare provider operations", a.Code)
	}
	for _, operation := range a.ProviderOperations {
		if operation == "" {
			return fmt.Errorf("action %s provider operation is required", a.Code)
		}
	}
	return nil
}

func (r ActionRegistry) Validate() error {
	if len(r.actions) == 0 {
		return errors.New("action registry is empty")
	}
	for code, action := range r.actions {
		if code != action.Code {
			return fmt.Errorf("action registry key %s does not match action code %s", code, action.Code)
		}
		if err := action.Validate(); err != nil {
			return err
		}
	}
	return nil
}
