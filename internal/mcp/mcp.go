// Package mcp implements Kiro's server-side MCP web-search tool support.
//
// Kiro exposes a "web_search" tool that executes on its own infrastructure via
// an MCP (Model Context Protocol) JSON-RPC endpoint ({q_host}/mcp). This package
// provides:
//
//   - The web_search tool definition and its conditional injection into a
//     request's tool list, gated by config.Config.WebSearchEnabled (Path B,
//     "MCP emulation": the model is offered a web_search tool whose calls are
//     intercepted by the streaming layer).
//   - Detection of a native, client-supplied server-side web_search tool
//     (Path A: bypass the model entirely and call the MCP API directly).
//   - The Kiro MCP API call (CallKiroMCPAPI) and the human-readable summary
//     generator (GenerateSearchSummary).
//   - SSE and JSON response emulation in both the Anthropic and OpenAI shapes,
//     including the high-level Path A entry point HandleNativeWebSearch.
//
// To avoid coupling to the route/auth layers (and any import cycle), the Kiro
// call is made through the small AuthManager interface, which *auth.KiroAuthManager
// satisfies directly. Bug #5: every Kiro call must carry the account's profile
// ARN, so the supplied AuthManager.ProfileARN must return it — CallKiroMCPAPI
// attaches it to the MCP request when non-empty.
package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"git.gasi.dev/robin/kiro-proxy/internal/debuglog"
	"git.gasi.dev/robin/kiro-proxy/internal/tokenizer"
	"git.gasi.dev/robin/kiro-proxy/internal/util"
)

// WebSearchToolName is the tool name used for both the injected emulation tool
// and the native server-side tool.
const WebSearchToolName = "web_search"

// WebSearchToolDescription is the description attached to the auto-injected
// web_search tool (Path B / MCP emulation).
const WebSearchToolDescription = "Search the web for current information. Use when you need up-to-date data from the internet."

// API format identifiers accepted by HandleNativeWebSearch.
const (
	APIFormatAnthropic = "anthropic"
	APIFormatOpenAI    = "openai"
)

// queryPrefix is stripped from an extracted query when present; some clients
// prepend it before the actual search terms.
const queryPrefix = "Perform a web search for the query: "

// idAlphabet is the alphanumeric set used by GenerateRandomID (ASCII letters
// plus digits).
const idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// textChunkSize is the rune count of each text/content delta emitted by the SSE
// writers.
const textChunkSize = 100

// SearchResult is a single web-search hit returned by the Kiro MCP API.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	// PublishedDate is a Unix timestamp in milliseconds. It is a pointer so an
	// absent date (nil) is distinguishable from the zero epoch.
	PublishedDate *int64 `json:"publishedDate"`
}

// SearchResults is the parsed payload of the MCP response's
// result.content[0].text field (which is itself a JSON string, not an object).
type SearchResults struct {
	// Results is nil when the "results" key is absent or null, and a non-nil
	// empty slice when it is an empty array; GenerateSearchSummary relies on
	// this distinction when rendering the summary.
	Results      []SearchResult `json:"results"`
	TotalResults int            `json:"totalResults"`
	Query        string         `json:"query"`
}

// AuthManager is the subset of *auth.KiroAuthManager that the web-search path
// needs. *auth.KiroAuthManager satisfies it directly, so callers pass their
// account's auth manager as-is.
type AuthManager interface {
	// GetAccessToken returns a valid bearer token, refreshing if necessary.
	GetAccessToken(ctx context.Context) (string, error)
	// QHost returns the Q API host (e.g. "https://runtime.us-east-1.kiro.dev").
	// The MCP endpoint is QHost() + "/mcp".
	QHost() string
	// ProfileARN returns the AWS CodeWhisperer profile ARN. Bug #5: the MCP call
	// must carry this ARN, so callers must supply an AuthManager whose ProfileARN
	// is populated.
	ProfileARN() string
}

// Client performs Kiro MCP API calls. It bundles the shared HTTP client and a
// logger. The zero value is usable (it falls back to http.DefaultClient and
// slog.Default()), but prefer NewClient.
//
// One Client is shared by every request, so the raw MCP request/response capture
// is resolved per call from the context (see debuglog.FromContext) rather than
// held in a field.
type Client struct {
	// HTTP is the shared upstream HTTP client (see internal/transport). When nil,
	// http.DefaultClient is used.
	HTTP *http.Client
	// Logger receives diagnostic logs. When nil, slog.Default() is used.
	Logger *slog.Logger
}

// NewClient returns a Client using the given HTTP client and logger (either may
// be nil to use the defaults).
func NewClient(httpClient *http.Client, logger *slog.Logger) *Client {
	return &Client{HTTP: httpClient, Logger: logger}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// GenerateRandomID returns a random alphanumeric string of length n (n <= 0
// yields "").
func GenerateRandomID(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// ExtractQueryFromText derives a web-search query from a message's text. It
// strips the optional "Perform a web search for the query: " prefix and trims
// surrounding whitespace, returning "" when nothing usable remains. Callers
// typically pass the first user message's text (e.g. anthropic
// MessagesRequest.Messages[0].Content.Text()).
func ExtractQueryFromText(text string) string {
	if after, ok := strings.CutPrefix(text, queryPrefix); ok {
		text = after
	}
	return strings.TrimSpace(text)
}

// AnthropicWebSearchTool returns a fresh Anthropic-format web_search tool
// definition (the emulation tool injected for Path B).
func AnthropicWebSearchTool() map[string]any {
	return map[string]any{
		"name":         WebSearchToolName,
		"description":  WebSearchToolDescription,
		"input_schema": webSearchSchema(),
	}
}

// OpenAIWebSearchTool returns a fresh OpenAI-format web_search tool definition.
func OpenAIWebSearchTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        WebSearchToolName,
			"description": WebSearchToolDescription,
			"parameters":  webSearchSchema(),
		},
	}
}

func webSearchSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Search query"},
		},
		"required": []string{"query"},
	}
}

// InjectAnthropicWebSearchTool appends the Anthropic web_search emulation tool
// to tools when enabled is true and no tool named "web_search" is already
// present, returning the (possibly extended) slice. When enabled is false it
// returns tools unchanged. enabled should be config.Config.WebSearchEnabled.
func InjectAnthropicWebSearchTool(tools []map[string]any, enabled bool) []map[string]any {
	if !enabled {
		return tools
	}
	for _, t := range tools {
		if name, _ := t["name"].(string); name == WebSearchToolName {
			return tools
		}
	}
	return append(tools, AnthropicWebSearchTool())
}

// InjectOpenAIWebSearchTool appends the OpenAI web_search emulation tool to
// tools when enabled is true and no function tool named "web_search" is already
// present, returning the (possibly extended) slice. When enabled is false it
// returns tools unchanged. enabled should be config.Config.WebSearchEnabled.
func InjectOpenAIWebSearchTool(tools []map[string]any, enabled bool) []map[string]any {
	if !enabled {
		return tools
	}
	for _, t := range tools {
		if typ, _ := t["type"].(string); typ != "function" {
			continue
		}
		fn, _ := t["function"].(map[string]any)
		if name, _ := fn["name"].(string); name == WebSearchToolName {
			return tools
		}
	}
	return append(tools, OpenAIWebSearchTool())
}

// HasNativeWebSearch reports whether tools contains a native, client-supplied
// server-side web_search tool, identified by a "type" beginning with
// "web_search" (e.g. "web_search_20250305"). When true the caller should take
// Path A (HandleNativeWebSearch) instead of the MCP-emulation path. The injected
// emulation tools (which carry no such "type") are deliberately not matched.
func HasNativeWebSearch(tools []map[string]any) bool {
	for _, t := range tools {
		if typ, _ := t["type"].(string); strings.HasPrefix(typ, WebSearchToolName) {
			return true
		}
	}
	return false
}

// GenerateSearchSummary renders human-readable search results wrapped in
// <web_search>...</web_search> tags. Snippets are emitted in full (never
// truncated) so the model receives complete information. The "No results found."
// text is emitted only when the results object lacks a results array (a
// present-but-empty array yields no body and no "No results found." line).
func GenerateSearchSummary(query string, results *SearchResults) string {
	var b strings.Builder
	b.WriteString("\n<web_search>\nSearch results for \"")
	b.WriteString(query)
	b.WriteString("\":\n\n")

	if results != nil && results.Results != nil {
		for i, r := range results.Results {
			fmt.Fprintf(&b, "%d. Title: **%s**\n", i+1, r.Title)
			if r.PublishedDate != nil && *r.PublishedDate != 0 {
				dt := time.UnixMilli(*r.PublishedDate)
				b.WriteString("   Published: ")
				b.WriteString(dt.Format("02 Jan 2006 15:04:05"))
				b.WriteByte('\n')
			}
			if r.URL != "" {
				b.WriteString("   URL: ")
				b.WriteString(r.URL)
				b.WriteByte('\n')
			}
			if r.Snippet != "" {
				b.WriteString("   ")
				b.WriteString(r.Snippet)
				b.WriteByte('\n')
			}
			b.WriteByte('\n')
		}
	} else {
		b.WriteString("No results found.\n")
	}

	b.WriteString("</web_search>\n")
	return b.String()
}

// CallKiroMCPAPI calls the Kiro MCP API for a web_search query. It returns a
// server tool-use id ("srvtoolu_...") and the parsed results. On any failure
// (token, transport, non-200 status, JSON-RPC error, or malformed body) it
// returns a non-nil error and nil results, leaving error handling to the caller.
//
// The call uses a 60-second timeout derived from ctx, and (Bug #5) attaches the
// AuthManager's profile ARN to the request when non-empty.
func (c *Client) CallKiroMCPAPI(ctx context.Context, am AuthManager, query string) (toolUseID string, results *SearchResults, err error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	requestID := fmt.Sprintf("web_search_tooluse_%s_%d_%s",
		GenerateRandomID(22), time.Now().UnixMilli(), GenerateRandomID(8))
	toolUseID = "srvtoolu_" + util.UUIDHex()

	mcpRequest := map[string]any{
		"id":      requestID,
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      WebSearchToolName,
			"arguments": map[string]any{"query": query},
		},
	}
	// Bug #5: every Kiro call must carry the profile ARN. The MCP request omitted
	// it, so mirror the main payload by attaching it at the top level.
	if arn := am.ProfileARN(); arn != "" {
		mcpRequest["profileArn"] = arn
	}

	reqBody, err := marshalNoEscape(mcpRequest)
	if err != nil {
		return "", nil, fmt.Errorf("mcp api: encode request: %w", err)
	}
	// Capture the raw MCP exchange; a no-op unless DEBUG_MODE is on.
	dbg := debuglog.FromContext(ctx)
	if pretty, e := json.MarshalIndent(mcpRequest, "", "  "); e == nil {
		dbg.LogRawChunk(append([]byte("[MCP REQUEST]\n"), pretty...))
	}

	token, err := am.GetAccessToken(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("mcp api: get access token: %w", err)
	}

	url := am.QHost() + "/mcp"
	c.logger().Debug("calling MCP API", "url", url)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", nil, fmt.Errorf("mcp api: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("x-amzn-codewhisperer-optout", "false")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		c.logger().Error("MCP API request error", "err", err)
		return "", nil, fmt.Errorf("mcp api: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", nil, fmt.Errorf("mcp api: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		c.logger().Error("MCP API error", "status", resp.StatusCode)
		return "", nil, fmt.Errorf("mcp api: unexpected status %d", resp.StatusCode)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		dbg.LogRawChunk(append([]byte("[MCP RESPONSE]\n"), pretty.Bytes()...))
	}

	var mr struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		c.logger().Error("MCP API response JSON parse error", "err", err)
		return "", nil, fmt.Errorf("mcp api: decode response: %w", err)
	}
	if len(mr.Error) > 0 && !bytes.Equal(bytes.TrimSpace(mr.Error), []byte("null")) {
		c.logger().Error("MCP API returned error", "error", string(mr.Error))
		return "", nil, fmt.Errorf("mcp api: error in response: %s", string(mr.Error))
	}

	// CRITICAL: result.content[0].text is itself a JSON string, not an object.
	resultText := "{}"
	if len(mr.Result.Content) > 0 && mr.Result.Content[0].Text != "" {
		resultText = mr.Result.Content[0].Text
	}
	var parsed SearchResults
	if err := json.Unmarshal([]byte(resultText), &parsed); err != nil {
		c.logger().Error("MCP API result text parse error", "err", err)
		return "", nil, fmt.Errorf("mcp api: parse result text: %w", err)
	}

	c.logger().Debug("MCP API returned results", "total", parsed.TotalResults)
	return toolUseID, &parsed, nil
}

// NativeWebSearchParams carries the inputs for HandleNativeWebSearch (Path A).
type NativeWebSearchParams struct {
	// Model is the requested model name, echoed in the response.
	Model string
	// Query is the search query, already extracted by the caller (see
	// ExtractQueryFromText). An empty Query yields an HTTP 400.
	Query string
	// Messages is the request's messages in plain map form (e.g. each message's
	// model_dump equivalent), used only to estimate input tokens.
	Messages []map[string]any
	// Stream selects SSE (true) or a single JSON body (false).
	Stream bool
	// APIFormat is APIFormatAnthropic (default) or APIFormatOpenAI.
	APIFormat string
}

// HandleNativeWebSearch implements Path A: it calls the MCP API directly and
// writes the complete client response to w, bypassing the model. It writes an
// Anthropic-style error body on failure (HTTP 400 for a missing query, HTTP 500
// for an MCP failure) and HTTP 200 with the emulated response otherwise. The
// returned error reflects only response-writing failures.
func (c *Client) HandleNativeWebSearch(ctx context.Context, w http.ResponseWriter, am AuthManager, p NativeWebSearchParams) error {
	if p.Query == "" {
		return writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "Cannot extract search query from messages")
	}
	c.logger().Info("native web_search (Path A)", "query", p.Query)

	toolUseID, results, err := c.CallKiroMCPAPI(ctx, am, p.Query)
	if err != nil || results == nil {
		c.logger().Error("native web_search failed", "err", err)
		return writeJSONError(w, http.StatusInternalServerError, "api_error", "Web search failed. Please try again.")
	}

	inputTokens := tokenizer.CountMessageTokens(p.Messages, false)

	if p.Stream {
		setSSEHeaders(w)
		if p.APIFormat == APIFormatOpenAI {
			return WriteOpenAIWebSearchSSE(w, p.Model, p.Query, toolUseID, results, inputTokens)
		}
		return WriteAnthropicWebSearchSSE(w, p.Model, p.Query, toolUseID, results, inputTokens)
	}

	if p.APIFormat == APIFormatOpenAI {
		return writeJSON(w, http.StatusOK, BuildOpenAIWebSearchResponse(p.Model, p.Query, toolUseID, results, inputTokens))
	}
	return writeJSON(w, http.StatusOK, BuildAnthropicWebSearchResponse(p.Model, p.Query, toolUseID, results, inputTokens))
}

// BuildAnthropicWebSearchResponse builds the non-streaming Anthropic message
// body for a web_search response (server_tool_use + web_search_tool_result +
// text summary). Output tokens are counted from the summary without the Claude
// correction factor (this is MCP output, not model generation).
func BuildAnthropicWebSearchResponse(model, query, toolUseID string, results *SearchResults, inputTokens int) map[string]any {
	summary := GenerateSearchSummary(query, results)
	outputTokens := tokenizer.CountTokensWithCorrection(summary, false)

	return map[string]any{
		"id":   util.GenerateMessageID(),
		"type": "message",
		"role": "assistant",
		"content": []any{
			map[string]any{
				"type":  "server_tool_use",
				"id":    toolUseID,
				"name":  WebSearchToolName,
				"input": map[string]any{"query": query},
			},
			map[string]any{
				"type":        "web_search_tool_result",
				"tool_use_id": toolUseID,
				"content":     searchResultBlocks(results),
			},
			map[string]any{"type": "text", "text": summary},
		},
		"model":         model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens},
	}
}

// BuildOpenAIWebSearchResponse builds the non-streaming OpenAI chat.completion
// body for a web_search response. The query and toolUseID are unused (OpenAI has
// no server-tool concept) but kept for signature symmetry with the Anthropic
// builder. Output tokens are counted from the summary without the Claude
// correction factor.
func BuildOpenAIWebSearchResponse(model, query, toolUseID string, results *SearchResults, inputTokens int) map[string]any {
	summary := GenerateSearchSummary(query, results)
	outputTokens := tokenizer.CountTokensWithCorrection(summary, false)

	return map[string]any{
		"id":      util.GenerateCompletionID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": summary},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		},
	}
}

// WriteAnthropicWebSearchSSE writes the Anthropic SSE event stream emulating a
// web_search response: message_start, the server_tool_use block, the
// web_search_tool_result block, the streamed text summary, then message_delta
// and message_stop. It flushes after each event when w is an http.Flusher and
// returns on the first write error.
func WriteAnthropicWebSearchSSE(w io.Writer, model, query, toolUseID string, results *SearchResults, inputTokens int) error {
	summary := GenerateSearchSummary(query, results)
	outputTokens := tokenizer.CountTokensWithCorrection(summary, false)

	queryJSON, err := marshalNoEscape(map[string]string{"query": query})
	if err != nil {
		return err
	}

	// 1. message_start
	if err := writeAnthropicSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          util.GenerateMessageID(),
			"type":        "message",
			"role":        "assistant",
			"model":       model,
			"content":     []any{},
			"stop_reason": nil,
			"usage":       map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}

	// 2-4. server_tool_use block.
	if err := writeAnthropicSSE(w, "content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"id":    toolUseID,
			"type":  "server_tool_use",
			"name":  WebSearchToolName,
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	if err := writeAnthropicSSE(w, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": string(queryJSON)},
	}); err != nil {
		return err
	}
	if err := writeAnthropicSSE(w, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	}); err != nil {
		return err
	}

	// 5-6. web_search_tool_result block.
	if err := writeAnthropicSSE(w, "content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     searchResultBlocks(results),
		},
	}); err != nil {
		return err
	}
	if err := writeAnthropicSSE(w, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 1,
	}); err != nil {
		return err
	}

	// 7. text block start.
	if err := writeAnthropicSSE(w, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         2,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	// 8..N. summary text deltas.
	for _, chunk := range chunkRunes(summary, textChunkSize) {
		if err := writeAnthropicSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 2,
			"delta": map[string]any{"type": "text_delta", "text": chunk},
		}); err != nil {
			return err
		}
	}
	// N+1. text block stop.
	if err := writeAnthropicSSE(w, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 2,
	}); err != nil {
		return err
	}

	// N+2. message_delta with stop reason and output tokens.
	if err := writeAnthropicSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	}); err != nil {
		return err
	}
	// N+3. message_stop.
	return writeAnthropicSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

// WriteOpenAIWebSearchSSE writes the OpenAI SSE event stream emulating a
// web_search response: a role delta, the streamed summary content, a final
// finish_reason+usage chunk, then "data: [DONE]". It emulates server-side
// execution (the summary is returned as content, with no tool_calls flow). It
// flushes after each chunk when w is an http.Flusher and returns on the first
// write error. The toolUseID is unused (kept for signature symmetry).
func WriteOpenAIWebSearchSSE(w io.Writer, model, query, toolUseID string, results *SearchResults, inputTokens int) error {
	summary := GenerateSearchSummary(query, results)
	outputTokens := tokenizer.CountTokensWithCorrection(summary, false)

	completionID := util.GenerateCompletionID()
	created := time.Now().Unix()

	base := func(delta map[string]any, finishReason any) map[string]any {
		return map[string]any{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{
				map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason},
			},
		}
	}

	// Role delta.
	if err := writeOpenAIData(w, base(map[string]any{"role": "assistant"}, nil)); err != nil {
		return err
	}
	// Content deltas.
	for _, chunk := range chunkRunes(summary, textChunkSize) {
		if err := writeOpenAIData(w, base(map[string]any{"content": chunk}, nil)); err != nil {
			return err
		}
	}
	// Finish chunk with usage.
	final := base(map[string]any{}, "stop")
	final["usage"] = map[string]any{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
	}
	if err := writeOpenAIData(w, final); err != nil {
		return err
	}

	_, err := io.WriteString(w, "data: [DONE]\n\n")
	flush(w)
	return err
}

// SearchResultBlocks converts results into Anthropic web_search_result content
// blocks (always a non-nil slice). It is exported for the route layer's
// mid-stream Path B interception, which must emit the same block shape that
// HandleNativeWebSearch / WriteAnthropicWebSearchSSE produce.
func SearchResultBlocks(results *SearchResults) []map[string]any {
	return searchResultBlocks(results)
}

// ChunkSummary splits a generated search summary into the same fixed-size rune
// chunks the SSE writers use, so the route layer can stream an intercepted
// summary identically to the Path A writers.
func ChunkSummary(summary string) []string {
	return chunkRunes(summary, textChunkSize)
}

// searchResultBlocks converts results into Anthropic web_search_result content
// blocks (always a non-nil slice so it marshals as [] rather than null).
func searchResultBlocks(results *SearchResults) []map[string]any {
	blocks := []map[string]any{}
	if results == nil {
		return blocks
	}
	for _, r := range results.Results {
		blocks = append(blocks, map[string]any{
			"type":              "web_search_result",
			"title":             r.Title,
			"url":               r.URL,
			"encrypted_content": r.Snippet,
			"page_age":          nil,
		})
	}
	return blocks
}

// chunkRunes splits s into substrings of at most size runes (code points), so
// multi-byte text is never split mid-rune. An empty s yields no chunks.
func chunkRunes(s string, size int) []string {
	if s == "" || size <= 0 {
		return nil
	}
	runes := []rune(s)
	out := make([]string, 0, (len(runes)+size-1)/size)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// writeAnthropicSSE writes one "event: <type>\ndata: <json>\n\n" frame and
// flushes when possible.
func writeAnthropicSSE(w io.Writer, eventType string, data map[string]any) error {
	payload, err := marshalNoEscape(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload); err != nil {
		return err
	}
	flush(w)
	return nil
}

// writeOpenAIData writes one "data: <json>\n\n" frame and flushes when possible.
func writeOpenAIData(w io.Writer, data map[string]any) error {
	payload, err := marshalNoEscape(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	flush(w)
	return nil
}

// flush flushes w when it implements http.Flusher (so SSE frames reach the
// client immediately).
func flush(w io.Writer) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// setSSEHeaders sets the event-stream headers and writes a 200 status.
func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
}

// writeJSON writes v as a JSON body with the given status. JSON is emitted
// without HTML escaping so the <web_search> tags in the summary survive intact.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	payload, err := marshalNoEscape(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(payload)
	return err
}

// writeJSONError writes an Anthropic-style error body with the given status.
func writeJSONError(w http.ResponseWriter, status int, errType, message string) error {
	return writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

// marshalNoEscape marshals v to compact JSON without HTML escaping, so literal
// characters like '<' and '>' are preserved.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
