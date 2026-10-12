package anthropic

import "strings"

// stopMatcher holds only the suffix that could still become a stop sequence
// after the next chunk. It therefore delays at most maxSequenceLength-1 bytes.
type stopMatcher struct {
	sequences []string
	pending   string
	maxLen    int
	stopped   bool
}

func newStopMatcher(sequences []string) *stopMatcher {
	m := &stopMatcher{}
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		m.sequences = append(m.sequences, sequence)
		if len(sequence) > m.maxLen {
			m.maxLen = len(sequence)
		}
	}
	if len(m.sequences) == 0 {
		return nil
	}
	return m
}

func (m *stopMatcher) Feed(chunk string) (visible, matched string) {
	if m == nil || m.stopped {
		return chunk, ""
	}
	m.pending += chunk
	matchAt := -1
	for _, sequence := range m.sequences {
		if at := strings.Index(m.pending, sequence); at >= 0 && (matchAt < 0 || at < matchAt) {
			matchAt, matched = at, sequence
		}
	}
	if matchAt >= 0 {
		m.stopped = true
		visible = m.pending[:matchAt]
		m.pending = ""
		return visible, matched
	}

	keep := m.maxLen - 1
	if keep < 0 {
		keep = 0
	}
	if len(m.pending) <= keep {
		return "", ""
	}
	visible = m.pending[:len(m.pending)-keep]
	m.pending = m.pending[len(m.pending)-keep:]
	return visible, ""
}

func (m *stopMatcher) Flush() string {
	if m == nil || m.stopped {
		return ""
	}
	visible := m.pending
	m.pending = ""
	return visible
}

func truncateAtStopSequence(text string, sequences []string) (visible, matched string) {
	matchAt := -1
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if at := strings.Index(text, sequence); at >= 0 && (matchAt < 0 || at < matchAt) {
			matchAt, matched = at, sequence
		}
	}
	if matchAt < 0 {
		return text, ""
	}
	return text[:matchAt], matched
}
