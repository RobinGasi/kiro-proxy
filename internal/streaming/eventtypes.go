package streaming

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
)

// eventTypeHeader is the AWS event-stream header that names each frame's type.
var eventTypeHeader = []byte(":event-type")

const (
	// headerValueString is the AWS event-stream header value type for strings.
	headerValueString = 7
	// maxEventTypeLen bounds accepted header values so arbitrary payload bytes
	// that happen to contain the header name cannot be mistaken for a header.
	maxEventTypeLen = 128
)

// eventTypeCounter tallies the ":event-type" headers of the AWS event-stream
// frames in one upstream response. It records header values only, never
// payloads, so stream failures can name unhandled event types without logging
// conversation content. Headers split across reads are counted once.
type eventTypeCounter struct {
	buf    []byte
	counts map[string]int
}

func (c *eventTypeCounter) feed(chunk []byte) {
	c.buf = append(c.buf, chunk...)
	keepFrom := -1
	pos := 0
	for {
		i := bytes.Index(c.buf[pos:], eventTypeHeader)
		if i < 0 {
			break
		}
		start := pos + i
		v := start + len(eventTypeHeader)
		// Header layout after the name: value type (1 byte), length (2 bytes BE), value.
		if v+3 > len(c.buf) {
			keepFrom = start
			break
		}
		n := int(binary.BigEndian.Uint16(c.buf[v+1 : v+3]))
		if c.buf[v] != headerValueString || n == 0 || n > maxEventTypeLen {
			pos = v
			continue
		}
		if v+3+n > len(c.buf) {
			keepFrom = start
			break
		}
		if c.counts == nil {
			c.counts = make(map[string]int)
		}
		c.counts[string(c.buf[v+3:v+3+n])]++
		pos = v + 3 + n
	}
	if keepFrom < 0 {
		// Keep only enough bytes to complete a header name split across reads.
		keepFrom = max(pos, len(c.buf)-(len(eventTypeHeader)-1))
	}
	c.buf = append(c.buf[:0], c.buf[keepFrom:]...)
}

// String renders the tally as "type=count" pairs sorted by type.
func (c *eventTypeCounter) String() string {
	if len(c.counts) == 0 {
		return ""
	}
	types := make([]string, 0, len(c.counts))
	for t := range c.counts {
		types = append(types, t)
	}
	sort.Strings(types)
	var b strings.Builder
	for i, t := range types {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(c.counts[t]))
	}
	return b.String()
}
