package provider

import "encoding/json"

// TrimToBudget reduces a transcript to fit a budget: the system messages stay,
// then the newest messages, as many as fit, going back in time. Everything
// older than the first one that does not fit is dropped, whatever it is:
// something said, thinking, a tool call or what a tool answered.
//
// It used to protect everything from the last user message onward and only
// shorten tool results in there. One question followed by a long run of tool
// steps is all one such turn, so its tool-call arguments and its thinking could
// never be dropped, and a request stayed the same size whatever the window was
// set to. Oldest goes first, with no exceptions for where a turn began.
//
// The one thing it will not do is separate a tool call from its answer. Every
// vendor rejects a tool result with no call before it, so an assistant message
// and the tool results that follow it go or stay together. When even the
// newest of those does not fit on its own, its tool results are cut short
// until it does, which is the only way left to fit without breaking the pair.
//
// The budget is expressed in characters of serialized JSON, which is what can
// be counted here without a tokenizer per vendor. Callers derive it from the
// model's context window, which is in the model's own tokens, at the rate the
// model's own counts have shown (CharsPerToken).
const (
	// GuessCharsPerToken is the rate for a model that has not reported a count
	// yet: its first call, or one whose vendor never says. On the safe side of
	// every model measured (3.5 to 4.4 characters a token), because the two
	// ways of being wrong are not alike: too low trims a little early, and too
	// high sends a request the model refuses.
	GuessCharsPerToken = 2.0

	// contextMargin is the share of the window kept free below what the rate
	// says fits, because the rate is learned from the calls before this one
	// and what a conversation holds shifts it a little.
	contextMargin = 0.05

	// truncationNotice marks a tool result that was cut, so the model can
	// tell the difference between a short result and a truncated one.
	truncationNotice = "\n...[truncated]"

	// minToolResultChars is the floor a truncated tool result keeps, so a
	// result never becomes meaningless noise.
	minToolResultChars = 200
)

// BudgetChars converts a context window in tokens into a character budget at
// charsPerToken, reserving room for the response itself and keeping the margin
// free.
func BudgetChars(contextWindow, reserveTokens int, charsPerToken float64) int {
	usable := contextWindow - reserveTokens
	if usable < 0 {
		usable = 0
	}
	return int(float64(usable) * charsPerToken * (1 - contextMargin))
}

// TrimToBudget returns the messages to send. It never mutates the input.
func TrimToBudget(messages []Message, budgetChars int) []Message {
	if budgetChars <= 0 || size(messages) <= budgetChars {
		return messages
	}

	// System messages are always kept, wherever they sit.
	systems, rest := splitSystem(messages)
	room := budgetChars - size(systems)

	// The newest steps that fit, counted back from the end.
	units := steps(rest)
	start, used := len(units), 0
	for start > 0 && used+size(units[start-1]) <= room {
		start--
		used += size(units[start])
	}
	// A tool result whose call was left behind cannot be sent.
	for start < len(units) && units[start][0].Role == RoleTool {
		start++
	}

	out := make([]Message, 0, len(messages))
	out = append(out, systems...)
	if start == len(units) {
		// Not even the newest step fits by itself. Keep it, with what its
		// tools answered cut down to the room there is.
		if newest := len(units) - 1; newest >= 0 && units[newest][0].Role != RoleTool {
			out = append(out, truncateToolResults(units[newest], room)...)
		}
		return out
	}
	for _, unit := range units[start:] {
		out = append(out, unit...)
	}
	return out
}

// steps groups messages into what can be dropped as one: a message on its own,
// or an assistant message together with the tool results that answer it. A
// tool result with no assistant message before it stays a unit of its own, so
// it can be recognised and never sent first.
func steps(messages []Message) [][]Message {
	var units [][]Message
	for _, m := range messages {
		if m.Role == RoleTool && len(units) > 0 {
			last := units[len(units)-1]
			if first := last[0].Role; first == RoleAssistant || first == RoleTool {
				units[len(units)-1] = append(last, m)
				continue
			}
		}
		units = append(units, []Message{m})
	}
	return units
}

func splitSystem(messages []Message) (systems, rest []Message) {
	for _, m := range messages {
		if m.Role == RoleSystem {
			systems = append(systems, m)
			continue
		}
		rest = append(rest, m)
	}
	return systems, rest
}

// truncateToolResults shrinks the largest tool results first, which frees
// the most space for the fewest edits.
func truncateToolResults(messages []Message, budgetChars int) []Message {
	out := make([]Message, len(messages))
	copy(out, messages)

	for size(out) > budgetChars {
		idx := largestToolResult(out)
		if idx < 0 {
			// Nothing left that may be shortened.
			return out
		}
		excess := size(out) - budgetChars
		content := out[idx].Content
		keep := len(content) - excess - len(truncationNotice)
		if keep < minToolResultChars {
			keep = minToolResultChars
		}
		if keep >= len(content) {
			// This result cannot give back any more space; stop rather than
			// spin forever.
			return out
		}
		out[idx].Content = content[:keep] + truncationNotice
	}
	return out
}

func largestToolResult(messages []Message) int {
	best, bestLen := -1, minToolResultChars+len(truncationNotice)
	for i, m := range messages {
		if m.Role != RoleTool {
			continue
		}
		if len(m.Content) > bestLen {
			best, bestLen = i, len(m.Content)
		}
	}
	return best
}

// size is the serialized cost of the messages. Marshaling failures cannot
// happen for these types, and a zero on failure would only make trimming
// more aggressive, never less safe.
func size(messages []Message) int {
	total := 0
	for _, m := range messages {
		raw, err := json.Marshal(m)
		if err != nil {
			continue
		}
		total += len(raw)
	}
	return total
}
