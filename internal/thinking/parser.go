// Package thinking implements a streaming-safe parser for "fake reasoning"
// blocks emitted by models that wrap their chain-of-thought in tags such as
// <thinking>, <think>, <reasoning> or <thought>.
//
// It is a small finite state machine designed to operate on the partial chunks
// produced by a streaming response: a tag may be split across several network
// chunks, so the parser buffers "cautiously" to avoid emitting a fragment of a
// tag as content. Detection happens ONLY at the start of the response; once a
// thinking block has been seen and closed, every subsequent chunk is treated as
// regular content (even if it contains more tags).
//
// An Enabled toggle (bug #178) lets callers run a pure pass-through for models
// that ignore the injected reasoning tags, keeping the parser
// capability-agnostic.
package thinking

import (
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"git.gasi.dev/robin/kiro-proxy/internal/config"
)

// Handling modes for thinking content, mirroring config.FakeReasoningHandling.
const (
	// HandleAsReasoningContent extracts thinking content so the caller can put
	// it in a dedicated reasoning_content field.
	HandleAsReasoningContent = "as_reasoning_content"
	// HandleRemove drops thinking content entirely.
	HandleRemove = "remove"
	// HandlePass re-emits thinking content with its original tags intact.
	HandlePass = "pass"
	// HandleStripTags emits thinking content as plain text without the tags.
	HandleStripTags = "strip_tags"
)

// Defaults mirror the corresponding config.FakeReasoning* defaults so the parser
// can be used standalone without a *config.Config.
const (
	// DefaultHandlingMode is used when Options.HandlingMode is empty.
	DefaultHandlingMode = HandleAsReasoningContent
	// DefaultInitialBufferSize is used when Options.InitialBufferSize <= 0.
	DefaultInitialBufferSize = 20
)

// DefaultOpenTags are the opening tags recognised when Options.OpenTags is
// empty. It is treated as read-only; New copies it into each parser.
var DefaultOpenTags = []string{"<thinking>", "<think>", "<reasoning>", "<thought>"}

// parserState enumerates the FSM states.
type parserState int

const (
	// statePreContent is the initial state: buffering to detect an opening tag.
	statePreContent parserState = iota
	// stateInThinking means we are inside a thinking block, buffering until the
	// closing tag is seen.
	stateInThinking
	// stateStreaming means no further thinking detection occurs; everything is
	// regular content.
	stateStreaming
)

// Result is the outcome of feeding one chunk through the parser. A field set to
// the empty string means "no delta of that kind"; the boolean flags qualify the
// thinking delta. Both ThinkingContent and RegularContent may be set in the same
// Result (e.g. when a closing tag is found mid-chunk).
type Result struct {
	// ThinkingContent is the raw thinking text extracted from this chunk. Pass
	// it through ProcessForOutput to apply the configured handling mode.
	ThinkingContent string
	// RegularContent is ordinary assistant content to forward to the client.
	RegularContent string
	// IsFirstThinkingChunk reports whether ThinkingContent is the first thinking
	// delta of the block (useful for prepending an opening tag in pass mode).
	IsFirstThinkingChunk bool
	// IsLastThinkingChunk reports whether the thinking block just closed.
	IsLastThinkingChunk bool
	// StateChanged reports whether the parser transitioned during this feed.
	StateChanged bool
}

// Options configures a Parser.
type Options struct {
	// Enabled toggles thinking-tag parsing. When false, Feed is a pure
	// pass-through (content is returned verbatim as RegularContent) and Finalize
	// yields nothing. This keeps the parser capability-agnostic: callers disable
	// it for models that ignore the injected reasoning tags (bug #178).
	Enabled bool
	// HandlingMode is one of the Handle* constants. Empty uses DefaultHandlingMode.
	HandlingMode string
	// OpenTags is the set of opening tags to detect. Empty uses DefaultOpenTags.
	OpenTags []string
	// InitialBufferSize caps how many characters are buffered while looking for
	// an opening tag. Values <= 0 use DefaultInitialBufferSize.
	InitialBufferSize int
}

// Parser is a finite state machine that separates thinking-block content from
// regular content in a streamed response. It is not safe for concurrent use;
// drive a single Parser from a single stream.
type Parser struct {
	enabled           bool
	handlingMode      string
	openTags          []string
	initialBufferSize int
	// maxTagLength is how many trailing characters are kept in the thinking
	// buffer to avoid splitting a closing tag across chunks.
	maxTagLength int

	state                parserState
	initialBuffer        string
	thinkingBuffer       string
	openTag              string
	closeTag             string
	isFirstThinkingChunk bool
	thinkingBlockFound   bool
}

// New builds a Parser from explicit options, applying defaults for any
// unset field.
func New(opts Options) *Parser {
	handling := opts.HandlingMode
	if handling == "" {
		handling = DefaultHandlingMode
	}

	tags := opts.OpenTags
	if len(tags) == 0 {
		tags = DefaultOpenTags
	}
	openTags := make([]string, len(tags))
	copy(openTags, tags)

	bufSize := opts.InitialBufferSize
	if bufSize <= 0 {
		bufSize = DefaultInitialBufferSize
	}

	// Buffer twice the longest opening tag so a closing tag (always shorter than
	// or equal to its opening tag) can never be split across cautious flushes.
	maxTagLen := 0
	for _, t := range openTags {
		if l := utf8.RuneCountInString(t); l > maxTagLen {
			maxTagLen = l
		}
	}
	maxTagLen *= 2

	p := &Parser{
		enabled:           opts.Enabled,
		handlingMode:      handling,
		openTags:          openTags,
		initialBufferSize: bufSize,
		maxTagLength:      maxTagLen,
	}
	p.Reset()
	return p
}

// NewFromConfig builds a Parser wired from the FakeReasoning* settings on cfg.
// The per-stream Enabled flag is taken from cfg.FakeReasoningEnabled; callers
// that need to disable parsing for a specific model (bug #178) should use New
// with Options.Enabled set explicitly.
func NewFromConfig(cfg *config.Config) *Parser {
	return New(Options{
		Enabled:           cfg.FakeReasoningEnabled,
		HandlingMode:      cfg.FakeReasoningHandling,
		OpenTags:          cfg.FakeReasoningOpenTags,
		InitialBufferSize: cfg.FakeReasoningInitialBuffer,
	})
}

// Enabled reports whether thinking-tag parsing is active.
func (p *Parser) Enabled() bool { return p.enabled }

// FoundThinkingBlock reports whether a thinking block was detected in this
// response.
func (p *Parser) FoundThinkingBlock() bool { return p.thinkingBlockFound }

// Feed processes a chunk of streamed content and returns the deltas it produced.
// Empty input yields an empty Result. When the parser is disabled the content is
// returned verbatim as RegularContent.
func (p *Parser) Feed(content string) Result {
	if content == "" {
		return Result{}
	}
	if !p.enabled {
		return Result{RegularContent: content}
	}

	var result Result
	if p.state == statePreContent {
		result = p.handlePreContent(content)
	}

	// If we are inside a thinking block and the pre-content handler did not
	// already consume this chunk (state did not change this call), process it as
	// thinking content.
	if p.state == stateInThinking && !result.StateChanged {
		result = p.handleInThinking(content)
	}

	// Plain streaming after the thinking block (or when none was found).
	if p.state == stateStreaming && !result.StateChanged {
		result.RegularContent = content
	}

	return result
}

// handlePreContent buffers content while looking for an opening tag at the very
// start of the response.
func (p *Parser) handlePreContent(content string) Result {
	var result Result
	p.initialBuffer += content

	// Leading whitespace is ignored when matching the opening tag.
	stripped := strings.TrimLeftFunc(p.initialBuffer, unicode.IsSpace)

	for _, tag := range p.openTags {
		if strings.HasPrefix(stripped, tag) {
			// Opening tag found: enter the thinking block.
			p.state = stateInThinking
			p.openTag = tag
			p.closeTag = "</" + tag[1:] // "<thinking>" -> "</thinking>"
			p.thinkingBlockFound = true
			result.StateChanged = true

			slog.Debug("thinking tag detected, transitioning to IN_THINKING", "tag", tag)

			p.thinkingBuffer = stripped[len(tag):]
			p.initialBuffer = ""

			// The remainder may already contain (part of) the closing tag.
			tr := p.processThinkingBuffer()
			if tr.ThinkingContent != "" {
				result.ThinkingContent = tr.ThinkingContent
				result.IsFirstThinkingChunk = tr.IsFirstThinkingChunk
			}
			if tr.IsLastThinkingChunk {
				result.IsLastThinkingChunk = true
			}
			if tr.RegularContent != "" {
				result.RegularContent = tr.RegularContent
			}
			return result
		}
	}

	// Still possibly receiving an opening tag: keep buffering.
	for _, tag := range p.openTags {
		if strings.HasPrefix(tag, stripped) && len(stripped) < len(tag) {
			return result
		}
	}

	// Either the buffer grew past the limit or it can no longer be a tag prefix:
	// there is no thinking block, so flush everything as regular content.
	if utf8.RuneCountInString(p.initialBuffer) > p.initialBufferSize || !p.couldBeTagPrefix(stripped) {
		p.state = stateStreaming
		result.StateChanged = true
		result.RegularContent = p.initialBuffer
		p.initialBuffer = ""
		slog.Debug("no thinking tag detected, transitioning to STREAMING")
	}

	return result
}

// couldBeTagPrefix reports whether text could be the start of any opening tag.
func (p *Parser) couldBeTagPrefix(text string) bool {
	if text == "" {
		return true
	}
	for _, tag := range p.openTags {
		if strings.HasPrefix(tag, text) {
			return true
		}
	}
	return false
}

// handleInThinking appends content to the thinking buffer and processes it.
func (p *Parser) handleInThinking(content string) Result {
	p.thinkingBuffer += content
	return p.processThinkingBuffer()
}

// processThinkingBuffer scans the thinking buffer for the closing tag, emitting
// thinking content cautiously so a not-yet-complete closing tag is never split.
func (p *Parser) processThinkingBuffer() Result {
	var result Result
	if p.closeTag == "" {
		return result
	}

	if idx := strings.Index(p.thinkingBuffer, p.closeTag); idx >= 0 {
		// Closing tag found: emit the remaining thinking content and switch to
		// plain streaming.
		thinkingContent := p.thinkingBuffer[:idx]
		afterTag := p.thinkingBuffer[idx+len(p.closeTag):]

		if thinkingContent != "" {
			result.ThinkingContent = thinkingContent
			result.IsFirstThinkingChunk = p.isFirstThinkingChunk
			p.isFirstThinkingChunk = false
		}
		result.IsLastThinkingChunk = true

		p.state = stateStreaming
		result.StateChanged = true
		p.thinkingBuffer = ""

		slog.Debug("closing tag found, transitioning to STREAMING", "tag", p.closeTag)

		// Drop the whitespace/newlines that usually trail the closing tag.
		if afterTag != "" {
			if strippedAfter := strings.TrimLeftFunc(afterTag, unicode.IsSpace); strippedAfter != "" {
				result.RegularContent = strippedAfter
			}
		}
		return result
	}

	// No closing tag yet: keep the last maxTagLength characters buffered so a
	// closing tag arriving across chunk boundaries is not split.
	runes := []rune(p.thinkingBuffer)
	if len(runes) > p.maxTagLength {
		n := len(runes) - p.maxTagLength
		result.ThinkingContent = string(runes[:n])
		result.IsFirstThinkingChunk = p.isFirstThinkingChunk
		p.isFirstThinkingChunk = false
		p.thinkingBuffer = string(runes[n:])
	}

	return result
}

// Finalize flushes any buffered content when the stream ends and returns the
// trailing deltas. It is a no-op when the parser is disabled.
func (p *Parser) Finalize() Result {
	var result Result

	if p.thinkingBuffer != "" {
		if p.state == stateInThinking {
			result.ThinkingContent = p.thinkingBuffer
			result.IsFirstThinkingChunk = p.isFirstThinkingChunk
			result.IsLastThinkingChunk = true
			slog.Warn("stream ended while still in thinking block; flushing remaining content")
		} else {
			result.RegularContent = p.thinkingBuffer
		}
		p.thinkingBuffer = ""
	}

	// If a tag was never found we may still hold buffered pre-content.
	if p.initialBuffer != "" {
		result.RegularContent += p.initialBuffer
		p.initialBuffer = ""
	}

	return result
}

// Reset returns the parser to its initial state so it can be reused for a new
// response.
func (p *Parser) Reset() {
	p.state = statePreContent
	p.initialBuffer = ""
	p.thinkingBuffer = ""
	p.openTag = ""
	p.closeTag = ""
	p.isFirstThinkingChunk = true
	p.thinkingBlockFound = false
}

// ProcessForOutput converts a raw thinking delta into its final string form
// according to the configured handling mode. The boolean return is false when
// nothing should be emitted (empty input, or "remove" mode). isFirst and isLast
// control whether the original opening/closing tags are re-attached in "pass"
// mode.
func (p *Parser) ProcessForOutput(thinkingContent string, isFirst, isLast bool) (string, bool) {
	if thinkingContent == "" {
		return "", false
	}

	switch p.handlingMode {
	case HandleRemove:
		return "", false
	case HandlePass:
		var prefix, suffix string
		if isFirst && p.openTag != "" {
			prefix = p.openTag
		}
		if isLast && p.closeTag != "" {
			suffix = p.closeTag
		}
		return prefix + thinkingContent + suffix, true
	case HandleStripTags:
		return thinkingContent, true
	default: // HandleAsReasoningContent
		return thinkingContent, true
	}
}
