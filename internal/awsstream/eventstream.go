package awsstream

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"strings"
)

const (
	eventStreamPreludeLen  = 12
	eventStreamMinFrameLen = 16
	eventStreamMaxFrameLen = 16 << 20
)

type streamMode uint8

const (
	streamModeUnknown streamMode = iota
	streamModeBinary
	streamModeLegacyJSON
)

// FeedStream decodes Kiro's AWS EventStream envelope and routes each frame by
// its Smithy union member in :event-type. Plain JSON remains supported for
// focused tests and legacy upstreams.
func (p *Parser) FeedStream(chunk []byte) ([]Event, error) {
	p.frameBuffer = append(p.frameBuffer, chunk...)
	if p.streamMode == streamModeUnknown {
		if first, ok := firstNonSpace(p.frameBuffer); ok && (first == '{' || first == '[') {
			p.streamMode = streamModeLegacyJSON
		} else if len(p.frameBuffer) >= eventStreamPreludeLen {
			p.streamMode = streamModeBinary
		} else {
			return nil, nil
		}
	}
	if p.streamMode == streamModeLegacyJSON {
		buffered := p.frameBuffer
		p.frameBuffer = nil
		return p.Feed(buffered), nil
	}

	var events []Event
	for len(p.frameBuffer) >= eventStreamPreludeLen {
		total := int(binary.BigEndian.Uint32(p.frameBuffer[:4]))
		headersLen := int(binary.BigEndian.Uint32(p.frameBuffer[4:8]))
		if total < eventStreamMinFrameLen || total > eventStreamMaxFrameLen || headersLen > total-eventStreamMinFrameLen {
			return nil, fmt.Errorf("awsstream: invalid frame lengths total=%d headers=%d", total, headersLen)
		}
		if len(p.frameBuffer) < total {
			break
		}
		frame := p.frameBuffer[:total]
		if crc32.ChecksumIEEE(frame[:8]) != binary.BigEndian.Uint32(frame[8:12]) {
			return nil, fmt.Errorf("awsstream: prelude CRC mismatch")
		}
		if crc32.ChecksumIEEE(frame[:total-4]) != binary.BigEndian.Uint32(frame[total-4:]) {
			return nil, fmt.Errorf("awsstream: message CRC mismatch")
		}
		headers, err := decodeHeaders(frame[12 : 12+headersLen])
		if err != nil {
			return nil, err
		}
		payload := frame[12+headersLen : total-4]
		ev, ok, err := p.routeFrame(headers, payload)
		if err != nil {
			return nil, err
		}
		if ok {
			events = append(events, ev)
		}
		p.frameBuffer = p.frameBuffer[total:]
	}
	return events, nil
}

// FlushStream rejects incomplete frames and open tool calls instead of turning
// partial arguments into a successful empty object.
func (p *Parser) FlushStream() error {
	if p.streamMode == streamModeBinary && len(p.frameBuffer) != 0 {
		return fmt.Errorf("awsstream: truncated frame at EOF (%d bytes buffered)", len(p.frameBuffer))
	}
	for _, id := range p.streamToolOrder {
		if state := p.streamTools[id]; state != nil && !state.complete {
			return fmt.Errorf("awsstream: incomplete toolUseEvent at EOF for toolUseId %q", state.call.ID)
		}
	}
	return nil
}

func firstNonSpace(data []byte) (byte, bool) {
	for _, b := range data {
		if !strings.ContainsRune(" \t\r\n", rune(b)) {
			return b, true
		}
	}
	return 0, false
}

func decodeHeaders(raw []byte) (map[string]string, error) {
	headers := make(map[string]string)
	for len(raw) > 0 {
		nameLen := int(raw[0])
		raw = raw[1:]
		if nameLen == 0 || len(raw) < nameLen+1 {
			return nil, fmt.Errorf("awsstream: malformed header name")
		}
		name := string(raw[:nameLen])
		kind := raw[nameLen]
		raw = raw[nameLen+1:]
		var value string
		var n int
		switch kind {
		case 0, 1: // bool true/false
			if kind == 0 {
				value = "true"
			} else {
				value = "false"
			}
		case 2:
			n = 1
		case 3:
			n = 2
		case 4:
			n = 4
		case 5, 8:
			n = 8
		case 6, 7:
			if len(raw) < 2 {
				return nil, fmt.Errorf("awsstream: truncated header %q", name)
			}
			n = int(binary.BigEndian.Uint16(raw[:2]))
			raw = raw[2:]
		case 9:
			n = 16
		default:
			return nil, fmt.Errorf("awsstream: unsupported header type %d", kind)
		}
		if len(raw) < n {
			return nil, fmt.Errorf("awsstream: truncated header %q", name)
		}
		if kind == 7 {
			value = string(raw[:n])
		}
		raw = raw[n:]
		headers[name] = value
	}
	return headers, nil
}

func decodePayload(payload []byte) (map[string]any, error) {
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, fmt.Errorf("awsstream: invalid JSON payload: %w", err)
	}
	return data, nil
}

func (p *Parser) routeFrame(headers map[string]string, payload []byte) (Event, bool, error) {
	eventType := headers[":event-type"]
	messageType := headers[":message-type"]
	if messageType == "exception" || messageType == "error" {
		data, err := decodePayload(payload)
		if err != nil {
			return Event{}, false, err
		}
		if eventType == "" {
			eventType = headers[":exception-type"]
		}
		return Event{Type: EventError, UpstreamType: eventType, Data: data}, true, nil
	}
	if eventType == "" {
		return Event{}, false, fmt.Errorf("awsstream: event frame missing :event-type")
	}
	data, err := decodePayload(payload)
	if err != nil {
		return Event{}, false, err
	}
	switch eventType {
	case "assistantResponseEvent":
		ev, ok := p.processContentEvent(data)
		ev.UpstreamType = eventType
		return ev, ok, nil
	case "reasoningContentEvent":
		return Event{Type: EventReasoning, UpstreamType: eventType, Data: data}, true, nil
	case "toolUseEvent":
		if err := p.processToolFrame(data); err != nil {
			return Event{}, false, err
		}
		return Event{Type: EventIgnored, UpstreamType: eventType, Data: data}, true, nil
	case "meteringEvent":
		return Event{Type: EventUsage, UpstreamType: eventType, Data: data["usage"]}, true, nil
	case "contextUsageEvent":
		return Event{Type: EventContextUsage, UpstreamType: eventType, Data: data["contextUsagePercentage"]}, true, nil
	case "metadataEvent":
		return Event{Type: EventMetadata, UpstreamType: eventType, Data: data}, true, nil
	case "invalidStateEvent", "error", "throttlingError", "validationError", "serviceUnavailableError":
		return Event{Type: EventError, UpstreamType: eventType, Data: data}, true, nil
	default:
		return Event{Type: EventIgnored, UpstreamType: eventType, Data: data}, true, nil
	}
}
