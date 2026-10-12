package streaming

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"testing"
)

// eventFrameHeaders encodes the AWS event-stream headers carried by one frame.
func eventFrameHeaders(eventType string) []byte {
	var b []byte
	for _, h := range [][2]string{{":event-type", eventType}, {":content-type", "application/json"}, {":message-type", "event"}} {
		b = append(b, byte(len(h[0])))
		b = append(b, h[0]...)
		b = append(b, headerValueString)
		b = binary.BigEndian.AppendUint16(b, uint16(len(h[1])))
		b = append(b, h[1]...)
	}
	return b
}

func eventStreamFrame(messageType, eventType string, payload any) []byte {
	headers := [][2]string{{":message-type", messageType}, {":content-type", "application/json"}}
	if messageType == "exception" || messageType == "error" {
		headers = append(headers, [2]string{":exception-type", eventType})
	} else {
		headers = append(headers, [2]string{":event-type", eventType})
	}
	var encodedHeaders []byte
	for _, h := range headers {
		encodedHeaders = append(encodedHeaders, byte(len(h[0])))
		encodedHeaders = append(encodedHeaders, h[0]...)
		encodedHeaders = append(encodedHeaders, headerValueString)
		encodedHeaders = binary.BigEndian.AppendUint16(encodedHeaders, uint16(len(h[1])))
		encodedHeaders = append(encodedHeaders, h[1]...)
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

func TestEventTypeCounterCountsHeadersSplitAcrossReads(t *testing.T) {
	var stream []byte
	stream = append(stream, eventFrameHeaders("assistantResponseEvent")...)
	stream = append(stream, `{"content":"hi"}`...)
	stream = append(stream, eventFrameHeaders("reasoningContentEvent")...)
	stream = append(stream, `{"text":"x"}`...)
	stream = append(stream, eventFrameHeaders("assistantResponseEvent")...)
	stream = append(stream, `{"content":"there"}`...)

	var c eventTypeCounter
	for i := range stream {
		c.feed(stream[i : i+1])
	}
	if got, want := c.String(), "assistantResponseEvent=2,reasoningContentEvent=1"; got != want {
		t.Fatalf("event types = %q, want %q", got, want)
	}
}

func TestEventTypeCounterIgnoresHeaderNameInsidePayload(t *testing.T) {
	var c eventTypeCounter
	c.feed([]byte(`{"content":"the :event-type header names frames"}`))
	if got := c.String(); got != "" {
		t.Fatalf("event types = %q, want none for payload text", got)
	}
}
