package awsstream

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"strings"
	"testing"
)

func TestFeedStreamAssemblesRepeatedIdentityToolFragments(t *testing.T) {
	p := NewParser()
	frames := [][]byte{
		testEventStreamFrame("toolUseEvent", map[string]any{
			"name": "bash", "toolUseId": "tool-1", "input": `{"command":`,
		}),
		testEventStreamFrame("toolUseEvent", map[string]any{
			"name": "bash", "toolUseId": "tool-1", "input": `"pwd"`,
		}),
		testEventStreamFrame("toolUseEvent", map[string]any{
			"name": "bash", "toolUseId": "tool-1", "input": `}`, "stop": true,
		}),
	}
	for _, frame := range frames {
		if _, err := p.FeedStream(frame); err != nil {
			t.Fatalf("FeedStream: %v", err)
		}
	}
	calls := p.GetToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want one assembled call", calls)
	}
	if got, want := calls[0].ID, "tool-1"; got != want {
		t.Fatalf("id = %q, want %q", got, want)
	}
	if got, want := calls[0].Function.Name, "bash"; got != want {
		t.Fatalf("name = %q, want %q", got, want)
	}
	if got, want := calls[0].Function.Arguments, `{"command":"pwd"}`; got != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
}

func TestFeedStreamToolFrameBoundaries(t *testing.T) {
	t.Run("input-only continuation and stop-only frame", func(t *testing.T) {
		p := NewParser()
		feedTestFrames(t, p,
			map[string]any{"name": "read", "toolUseId": "tool-1", "input": `{"path":"`},
			map[string]any{"input": `README.md"}`},
			map[string]any{"toolUseId": "tool-1", "stop": true},
		)
		assertToolCalls(t, p.GetToolCalls(), []ToolCall{{
			ID: "tool-1", Type: "function",
			Function: ToolCallFunction{Name: "read", Arguments: `{"path":"README.md"}`},
		}})
	})

	t.Run("new identity keeps prior call open", func(t *testing.T) {
		p := NewParser()
		feedTestFrames(t, p,
			map[string]any{"name": "read", "toolUseId": "tool-1", "input": map[string]any{"path": "a.md"}},
			map[string]any{"name": "write", "toolUseId": "tool-2", "input": map[string]any{"path": "b.md"}, "stop": true},
			map[string]any{"toolUseId": "tool-1", "stop": true},
		)
		assertToolCalls(t, p.GetToolCalls(), []ToolCall{
			{ID: "tool-1", Type: "function", Function: ToolCallFunction{Name: "read", Arguments: `{"path":"a.md"}`}},
			{ID: "tool-2", Type: "function", Function: ToolCallFunction{Name: "write", Arguments: `{"path":"b.md"}`}},
		})
	})

	t.Run("duplicate terminal frame is deduplicated by identity", func(t *testing.T) {
		p := NewParser()
		frame := map[string]any{"name": "read", "toolUseId": "tool-1", "input": map[string]any{"path": "a.md"}, "stop": true}
		feedTestFrames(t, p, frame, frame)
		assertToolCalls(t, p.GetToolCalls(), []ToolCall{{
			ID: "tool-1", Type: "function",
			Function: ToolCallFunction{Name: "read", Arguments: `{"path":"a.md"}`},
		}})
	})
}

func TestFeedStreamRejectsInvalidToolIdentity(t *testing.T) {
	tests := []struct {
		name    string
		frames  []map[string]any
		wantErr string
	}{
		{
			name:    "empty event",
			frames:  []map[string]any{{}},
			wantErr: "empty toolUseEvent",
		},
		{
			name:    "invalid id",
			frames:  []map[string]any{{"name": "read", "toolUseId": " "}},
			wantErr: "invalid toolUseId",
		},
		{
			name:    "missing initial name",
			frames:  []map[string]any{{"toolUseId": "tool-1", "input": `{}`}},
			wantErr: "missing a tool name",
		},
		{
			name: "same id changes name",
			frames: []map[string]any{
				{"name": "read", "toolUseId": "tool-1", "input": `{"path":"`},
				{"name": "write", "toolUseId": "tool-1", "input": `a.md"}`},
			},
			wantErr: "tool name changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewParser()
			var err error
			for _, payload := range tt.frames {
				_, err = p.FeedStream(testEventStreamFrame("toolUseEvent", payload))
				if err != nil {
					break
				}
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestFeedStreamSupportsInterleavedToolCalls(t *testing.T) {
	p := NewParser()
	feedTestFrames(t, p,
		map[string]any{"name": "read", "toolUseId": "tool-1", "input": `{"path":"`},
		map[string]any{"name": "write", "toolUseId": "tool-2", "input": `{"path":"`},
		map[string]any{"name": "read", "toolUseId": "tool-1", "input": `a.md"}`, "stop": true},
		map[string]any{"name": "write", "toolUseId": "tool-2", "input": `b.md"}`, "stop": true},
	)
	assertToolCalls(t, p.GetToolCalls(), []ToolCall{
		{ID: "tool-1", Type: "function", Function: ToolCallFunction{Name: "read", Arguments: `{"path":"a.md"}`}},
		{ID: "tool-2", Type: "function", Function: ToolCallFunction{Name: "write", Arguments: `{"path":"b.md"}`}},
	})
}

func TestFeedStreamRekeysGeneratedToolID(t *testing.T) {
	p := NewParser()
	feedTestFrames(t, p,
		map[string]any{"name": "read", "input": `{"path":"`},
		map[string]any{"name": "read", "toolUseId": "upstream-id", "input": `a.md"}`, "stop": true},
	)
	assertToolCalls(t, p.GetToolCalls(), []ToolCall{{
		ID: "upstream-id", Type: "function",
		Function: ToolCallFunction{Name: "read", Arguments: `{"path":"a.md"}`},
	}})
}

func TestFeedStreamDeduplicatesObjectInputSnapshots(t *testing.T) {
	p := NewParser()
	snapshot := map[string]any{"path": "README.md"}
	feedTestFrames(t, p,
		map[string]any{"name": "read", "toolUseId": "tool-1", "input": snapshot},
		map[string]any{"name": "read", "toolUseId": "tool-1", "input": snapshot},
		map[string]any{"toolUseId": "tool-1", "stop": true},
	)
	assertToolCalls(t, p.GetToolCalls(), []ToolCall{{
		ID: "tool-1", Type: "function",
		Function: ToolCallFunction{Name: "read", Arguments: `{"path":"README.md"}`},
	}})
}

func TestFeedStreamTreatsEmptyObjectAsStartMarker(t *testing.T) {
	p := NewParser()
	feedTestFrames(t, p,
		map[string]any{"name": "read", "toolUseId": "tool-1", "input": map[string]any{}},
		map[string]any{"toolUseId": "tool-1", "input": `{"path":"README.md"}`},
		map[string]any{"toolUseId": "tool-1", "stop": true},
	)
	assertToolCalls(t, p.GetToolCalls(), []ToolCall{{
		ID: "tool-1", Type: "function",
		Function: ToolCallFunction{Name: "read", Arguments: `{"path":"README.md"}`},
	}})
}

func TestFlushStreamRejectsOpenToolCall(t *testing.T) {
	p := NewParser()
	feedTestFrames(t, p, map[string]any{
		"name": "read", "toolUseId": "tool-1", "input": `{"path":"README`,
	})
	if err := p.FlushStream(); err == nil || !strings.Contains(err.Error(), "incomplete toolUseEvent") {
		t.Fatalf("FlushStream error = %v, want incomplete toolUseEvent", err)
	}
	if calls := p.GetToolCalls(); len(calls) != 0 {
		t.Fatalf("calls = %#v, want no incomplete call", calls)
	}
}

func TestFeedStreamAssemblesToolCallAcrossArbitraryReads(t *testing.T) {
	var wire []byte
	for _, payload := range []map[string]any{
		{"name": "bash", "toolUseId": "tool-1", "input": `{"command":`},
		{"name": "bash", "toolUseId": "tool-1", "input": `"pwd"`},
		{"name": "bash", "toolUseId": "tool-1", "input": `}`, "stop": true},
	} {
		wire = append(wire, testEventStreamFrame("toolUseEvent", payload)...)
	}
	p := NewParser()
	for i := range wire {
		if _, err := p.FeedStream(wire[i : i+1]); err != nil {
			t.Fatalf("FeedStream byte %d: %v", i, err)
		}
	}
	assertToolCalls(t, p.GetToolCalls(), []ToolCall{{
		ID: "tool-1", Type: "function",
		Function: ToolCallFunction{Name: "bash", Arguments: `{"command":"pwd"}`},
	}})
}

func feedTestFrames(t *testing.T, p *Parser, payloads ...map[string]any) {
	t.Helper()
	for _, payload := range payloads {
		if _, err := p.FeedStream(testEventStreamFrame("toolUseEvent", payload)); err != nil {
			t.Fatalf("FeedStream: %v", err)
		}
	}
}

func TestParseTaggedToolCallsValidatesAdvertisedTools(t *testing.T) {
	text := "Reading now.\n<tool_use>{\"tool_name\":\"read_file\",\"tool_input\":{\"path\":\"README.md\"}}</tool_use>"
	cleaned, calls := ParseTaggedToolCalls(text, []string{"read"})
	if cleaned != "Reading now.\n" {
		t.Fatalf("cleaned = %q", cleaned)
	}
	assertToolCalls(t, calls, []ToolCall{{
		ID: calls[0].ID, Type: "function",
		Function: ToolCallFunction{Name: "read", Arguments: `{"path":"README.md"}`},
	}})
}

func TestParseTaggedToolCallsRejectsUnknownAndFencedCalls(t *testing.T) {
	unknown := `<tool_call>{"name":"delete_everything","arguments":{}}</tool_call>`
	if cleaned, calls := ParseTaggedToolCalls(unknown, []string{"read"}); cleaned != unknown || len(calls) != 0 {
		t.Fatalf("unknown call changed: %q %#v", cleaned, calls)
	}
	fenced := "```json\n<tool_use>{\"name\":\"read\",\"input\":{}}</tool_use>\n```"
	if cleaned, calls := ParseTaggedToolCalls(fenced, []string{"read"}); cleaned != fenced || len(calls) != 0 {
		t.Fatalf("fenced call changed: %q %#v", cleaned, calls)
	}
}

func TestParseTaggedToolCallsUnwrapsArraysAndMixedDialects(t *testing.T) {
	text := `<function_call>{"calls":[{"name":"read","arguments":{"path":"a"}},{"name":"write","input":{"path":"b","content":"x"}}]}</function_call>`
	cleaned, calls := ParseTaggedToolCalls(text, []string{"read", "write"})
	if cleaned != "" || len(calls) != 2 || calls[0].Function.Name != "read" || calls[1].Function.Name != "write" {
		t.Fatalf("cleaned=%q calls=%#v", cleaned, calls)
	}
}

func assertToolCalls(t *testing.T, got, want []ToolCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Type != want[i].Type || got[i].Function != want[i].Function {
			t.Fatalf("call %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func testEventStreamFrame(eventType string, payload any) []byte {
	headers := [][2]string{
		{":message-type", "event"},
		{":content-type", "application/json"},
		{":event-type", eventType},
	}
	var encodedHeaders []byte
	for _, header := range headers {
		encodedHeaders = append(encodedHeaders, byte(len(header[0])))
		encodedHeaders = append(encodedHeaders, header[0]...)
		encodedHeaders = append(encodedHeaders, 7)
		encodedHeaders = binary.BigEndian.AppendUint16(encodedHeaders, uint16(len(header[1])))
		encodedHeaders = append(encodedHeaders, header[1]...)
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	total := 12 + len(encodedHeaders) + len(encodedPayload) + 4
	frame := make([]byte, 12, total)
	binary.BigEndian.PutUint32(frame[:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(encodedHeaders)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, encodedHeaders...)
	frame = append(frame, encodedPayload...)
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	return frame
}
