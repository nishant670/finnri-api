package ai

import (
	"strings"
	"unicode/utf8"
)

// KnownNames are the user's own names for things a capture can point at: their
// events, split groups and friends.
//
// Without them the model only has the sentence. "Split with Bubu Dudu daily
// group" came back as a group called "daily group" plus two new friends,
// "Bubu" and "Dudu", and "add it to the Kanatal trip" had nowhere to go at all
// (2026-11-11). With the list, the model can pick the saved name, and the
// transcriber can spell it.
type KnownNames struct {
	Events  []string
	Groups  []string
	Friends []string
}

const (
	maxKnownEvents     = 30
	maxKnownGroups     = 40
	maxKnownFriends    = 60
	maxKnownNameRunes  = 40
	maxTranscribeHint  = 600
	transcribeHintHead = "A note about a payment. Names that may come up: "
)

func (k KnownNames) empty() bool {
	return len(k.Events) == 0 && len(k.Groups) == 0 && len(k.Friends) == 0
}

// cleanKnownNames trims, de-duplicates and caps a list. A name is the user's
// own text, so it is flattened onto one line and cut short: it is data for the
// model to match against, never something that can carry its own instructions.
func cleanKnownNames(names []string, limit int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.Join(strings.Fields(strings.NewReplacer(`"`, "", "\\", "").Replace(name)), " ")
		if name == "" {
			continue
		}
		if utf8.RuneCountInString(name) > maxKnownNameRunes {
			name = string([]rune(name)[:maxKnownNameRunes])
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
		if len(out) == limit {
			break
		}
	}
	return out
}

func (k KnownNames) cleaned() KnownNames {
	return KnownNames{
		Events:  cleanKnownNames(k.Events, maxKnownEvents),
		Groups:  cleanKnownNames(k.Groups, maxKnownGroups),
		Friends: cleanKnownNames(k.Friends, maxKnownFriends),
	}
}

func quotedList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = `"` + name + `"`
	}
	return strings.Join(quoted, ", ")
}

// contextLines is the block the parse request carries after the date line.
func (k KnownNames) contextLines() string {
	k = k.cleaned()
	if k.empty() {
		return ""
	}
	return "\nSaved events: " + quotedList(k.Events) +
		"\nSaved split groups: " + quotedList(k.Groups) +
		"\nSaved friends: " + quotedList(k.Friends)
}

// TranscriptionHint is the prompt the speech-to-text call is primed with, so a
// name the user has saved is spelled the way they saved it — "Bubu Dudu", not
// "Bobududu". Events and groups come first: they are the names most often
// misheard and the ones a wrong spelling cannot be matched back from.
func (k KnownNames) TranscriptionHint() string {
	k = k.cleaned()
	if k.empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString(transcribeHintHead)
	first := true
	for _, list := range [][]string{k.Events, k.Groups, k.Friends} {
		for _, name := range list {
			piece := name
			if !first {
				piece = ", " + name
			}
			if b.Len()+len(piece)+1 > maxTranscribeHint {
				b.WriteString(".")
				return b.String()
			}
			b.WriteString(piece)
			first = false
		}
	}
	b.WriteString(".")
	return b.String()
}
