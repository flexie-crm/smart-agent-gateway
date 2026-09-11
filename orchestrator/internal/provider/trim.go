package provider

import "encoding/json"

// TrimToBudget reduces a transcript to fit a token budget without ever
// producing a message list a vendor will reject.
//
// The naive approach (drop the oldest messages, keep the system prompt and
// the last few) is wrong, and wrong in a way that only shows up mid tool
// loop: at that moment the newest message is a tool result, and dropping
// the assistant turn that requested it leaves an orphaned tool message that
// every vendor rejects. So the rules are:
//
//  1. Anchor on the last user message. Everything from it to the end is the
//     active turn and is load bearing: it is never dropped.
//  2. Drop the oldest history before the anchor, and sweep any tool message
//     left orphaned at the front.
//  3. If the active turn alone still exceeds the budget, truncate the
//     largest tool results in place rather than dropping messages, which
//     preserves the assistant/tool pairing.
//
// The budget is expressed in characters of serialized JSON, which is a
// deliberate approximation: it is cheap, monotonic in real token count, and
// never needs a tokenizer per vendor. Callers derive it from the model's
// context window (see CharsPerToken).
const (
	// CharsPerToken is a conservative average across English text, code, and
	// JSON. Underestimating tokens would be dangerous, so this errs low.
	CharsPerToken = 3

	// truncationNotice marks a tool result that was cut, so the model can
	// tell the difference between a short result and a truncated one.
	truncationNotice = "\n...[truncated]"

	// minToolResultChars is the floor a truncated tool result keeps, so a
	// result never becomes meaningless noise.
	minToolResultChars = 200
)

// BudgetChars converts a context window in tokens into a character budget,
// reserving room for the response itself.
func BudgetChars(contextWindow, reserveTokens int) int {
	usable := contextWindow - reserveTokens
	if usable < 0 {
		usable = 0
	}
	return usable * CharsPerToken
}

// TrimToBudget returns the messages to send. It never mutates the input.
func TrimToBudget(messages []Message, budgetChars int) []Message {
	if budgetChars <= 0 || len(messages) <= 2 || size(messages) <= budgetChars {
		return messages
	}

	// System messages are always kept, wherever they sit.
	systems, rest := splitSystem(messages)

	anchor := lastUserIndex(rest)
	if anchor < 0 {
		// No user message at all: the whole thing is one turn, so the only
		// safe reduction is truncating tool results.
		return append(systems, truncateToolResults(rest, budgetChars-size(systems))...)
	}

	history, active := rest[:anchor], rest[anchor:]
	budget := budgetChars - size(systems) - size(active)

	// Drop history oldest first until what remains fits the leftover budget.
	for len(history) > 0 && (budget < 0 || size(history) > budget) {
		history = history[1:]
		history = dropLeadingOrphanTools(history)
	}

	out := make([]Message, 0, len(systems)+len(history)+len(active))
	out = append(out, systems...)
	out = append(out, history...)

	if budget < 0 {
		// The active turn alone is over budget: truncate its tool results
		// rather than break the assistant/tool pairing.
		active = truncateToolResults(active, budgetChars-size(systems))
	}
	return append(out, active...)
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

func lastUserIndex(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			return i
		}
	}
	return -1
}

// dropLeadingOrphanTools removes tool results left at the front with no
// assistant message requesting them.
func dropLeadingOrphanTools(messages []Message) []Message {
	for len(messages) > 0 && messages[0].Role == RoleTool {
		messages = messages[1:]
	}
	return messages
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
