package streaming

import (
	"strings"

	"git.gasi.dev/robin/kiro-proxy/internal/awsstream"
)

type taggedToolBuffer struct {
	tools    []string
	pending  string
	buffered bool
	inFence  bool
}

func newTaggedToolBuffer(tools []string) *taggedToolBuffer {
	if len(tools) == 0 {
		return nil
	}
	return &taggedToolBuffer{tools: tools}
}

// Feed returns text known not to belong to a tagged call. Once an opener or its
// suffix-prefix is seen, the remainder is held for validated EOF recovery.
func (b *taggedToolBuffer) Feed(content string) string {
	if b == nil {
		return content
	}
	if b.buffered {
		b.pending += content
		return ""
	}
	candidate := b.pending + content
	if at, fenced := firstToolOpenerOutsideFence(candidate, b.inFence); at >= 0 {
		b.buffered = true
		b.inFence = fenceState(candidate[:at], b.inFence)
		b.pending = candidate[at:]
		return candidate[:at]
	} else {
		b.inFence = fenced
	}
	keep := toolPrefixSuffix(candidate)
	visible := candidate[:len(candidate)-keep]
	b.pending = candidate[len(candidate)-keep:]
	return visible
}

func (b *taggedToolBuffer) Finalize() (string, []awsstream.ToolCall) {
	if b == nil {
		return "", nil
	}
	cleaned, calls := awsstream.ParseTaggedToolCalls(b.pending, b.tools)
	b.pending = ""
	return cleaned, calls
}

var taggedToolOpeners = []string{"<tool_use>", "<tool_call>", "<function_call>", "<tool>"}

func firstToolOpenerOutsideFence(text string, initial bool) (int, bool) {
	fenced := initial
	for index := 0; index < len(text); {
		if strings.HasPrefix(text[index:], "```") {
			fenced = !fenced
			index += 3
			continue
		}
		if !fenced {
			for _, opener := range taggedToolOpeners {
				if strings.HasPrefix(text[index:], opener) {
					return index, fenced
				}
			}
		}
		index++
	}
	return -1, fenced
}

func fenceState(text string, initial bool) bool {
	fenced := initial
	for offset := 0; ; {
		at := strings.Index(text[offset:], "```")
		if at < 0 {
			return fenced
		}
		fenced = !fenced
		offset += at + 3
	}
}

func toolPrefixSuffix(text string) int {
	best := 0
	for _, opener := range taggedToolOpeners {
		max := len(opener) - 1
		if len(text) < max {
			max = len(text)
		}
		for length := max; length > best; length-- {
			if strings.HasPrefix(opener, text[len(text)-length:]) {
				best = length
				break
			}
		}
	}
	return best
}
