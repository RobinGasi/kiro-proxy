package anthropic

import "strings"

// NormalizedRequest is the result of folding inline system messages into the
// system prompt, leaving only user/assistant turns for the converter.
type NormalizedRequest struct {
	System string    // top-level system first, then inline system parts, in order
	Turns  []Message // only role == "user" | "assistant" (others coerced to user)
}

// NormalizeRoles implements the headline fix: it accepts any role string,
// extracts role=="system" messages from messages[] and merges their text into
// the system prompt (top-level system first, then inline parts in order), and
// returns only user/assistant turns. Any other non-user/assistant role is
// coerced to "user" so the request never 422s on an unexpected role.
func (r *MessagesRequest) NormalizeRoles() NormalizedRequest {
	var sys []string
	if s := r.System.Text(); s != "" {
		sys = append(sys, s)
	}

	turns := make([]Message, 0, len(r.Messages))
	for _, m := range r.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "user", "assistant":
			turns = append(turns, Message{Role: role, Content: m.Content})
		case "system", "developer":
			if t := m.Content.Text(); t != "" {
				sys = append(sys, t)
			}
		default:
			// Unknown role: keep the content but treat the turn as user input
			// rather than rejecting the request.
			turns = append(turns, Message{Role: "user", Content: m.Content})
		}
	}

	return NormalizedRequest{
		System: strings.Join(sys, "\n\n"),
		Turns:  turns,
	}
}
