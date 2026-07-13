package payloadguard

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif" // register GIF decoder
	"image/jpeg"
	_ "image/png" // register PNG decoder

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder
)

const (
	// maxLongEdge caps the longest edge of a shrunk image on the first pass. It
	// mirrors Anthropic's own vision guidance (images are downsized so the long
	// edge is <= ~1568px), beyond which extra resolution buys the model nothing.
	maxLongEdge = 1568
	// minLongEdge is the floor the shrinker will not scale below; smaller than
	// this and screenshots become unreadable.
	minLongEdge = 256
	// jpegQuality / jpegQualityFloor bracket the re-encode quality.
	jpegQuality      = 80
	jpegQualityFloor = 60
	// shrinkMarginBytes / minB64Floor keep the per-image target away from zero so
	// a single shrink pass removes a meaningful chunk without over-shrinking.
	shrinkMarginBytes = 8 * 1024
	minB64Floor       = 8 * 1024
)

// candidateLongEdges is the descending sequence of longest-edge caps the
// shrinker tries until the encoded image fits the target.
var candidateLongEdges = []int{maxLongEdge, 1024, 768, 512, 384, minLongEdge}

// shrinkLargestImage downscales/re-encodes the largest image in the payload so
// the serialized request shrinks by at least excessBytes, mutating source.bytes
// (and its sibling format) in place. It returns true only when it actually made
// the stored image smaller, guaranteeing the caller's reduction loop makes
// progress or terminates.
func shrinkLargestImage(payload map[string]any, excessBytes int) bool {
	imgMap, src := largestImage(payload)
	if imgMap == nil {
		return false
	}

	b64, _ := src["bytes"].(string)
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return false
	}

	// Aim to remove at least excessBytes (of base64) from this one image, with a
	// safety margin, but never target below a small floor.
	desiredB64 := max(len(b64)-excessBytes-shrinkMarginBytes, minB64Floor)
	targetEncoded := desiredB64 * 3 / 4

	out, format, ok := shrinkImageBytes(raw, targetEncoded)
	if !ok {
		return false
	}

	newB64 := base64.StdEncoding.EncodeToString(out)
	if len(newB64) >= len(b64) {
		return false // no progress; fall through to stripping
	}
	src["bytes"] = newB64
	imgMap["format"] = format
	return true
}

// largestImage returns the image entry (and its source sub-map) with the largest
// base64 payload across the current message and history, or (nil, nil) when none
// exists.
func largestImage(payload map[string]any) (imgMap, src map[string]any) {
	largest := 0
	for _, im := range collectImageMaps(payload) {
		s, ok := im["source"].(map[string]any)
		if !ok {
			continue
		}
		b64, _ := s["bytes"].(string)
		if len(b64) > largest {
			largest = len(b64)
			imgMap = im
			src = s
		}
	}
	return imgMap, src
}

// collectImageMaps gathers every image entry map from the current message and
// history userInputMessages.
func collectImageMaps(payload map[string]any) []map[string]any {
	cs, ok := payload["conversationState"].(map[string]any)
	if !ok {
		return nil
	}

	var out []map[string]any
	add := func(user any) {
		um, ok := user.(map[string]any)
		if !ok {
			return
		}
		imgs, ok := um["images"].([]any)
		if !ok {
			return
		}
		for _, it := range imgs {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}

	if cur, ok := cs["currentMessage"].(map[string]any); ok {
		add(cur["userInputMessage"])
	}
	if hist, ok := cs["history"].([]any); ok {
		for _, e := range hist {
			if entry, ok := e.(map[string]any); ok {
				add(entry["userInputMessage"])
			}
		}
	}
	return out
}

// shrinkImageBytes decodes raw (PNG/JPEG/GIF/WebP), downscales the longest edge
// toward the byte target, and re-encodes as JPEG (much smaller than PNG for
// screenshots). It returns the smallest reasonable JPEG it can produce — at or
// under maxEncodedBytes when achievable, otherwise the floor-size/low-quality
// version — with format "jpeg". ok is false only when the image cannot be
// decoded.
func shrinkImageBytes(raw []byte, maxEncodedBytes int) (out []byte, format string, ok bool) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", false
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	var best []byte
	for _, edge := range candidateLongEdges {
		nw, nh := fitLongEdge(w, h, edge)
		scaled := img
		if nw != w || nh != h {
			scaled = resizeImage(img, nw, nh)
		}
		enc, err := encodeJPEG(scaled, jpegQuality)
		if err != nil {
			return nil, "", false
		}
		best = enc
		if len(enc) <= maxEncodedBytes {
			return enc, "jpeg", true
		}
		if nw <= 1 || nh <= 1 {
			break
		}
	}

	// Still over target: last resort is the floor size at the lowest quality.
	nw, nh := fitLongEdge(w, h, minLongEdge)
	scaled := img
	if nw != w || nh != h {
		scaled = resizeImage(img, nw, nh)
	}
	if enc, err := encodeJPEG(scaled, jpegQualityFloor); err == nil && len(enc) > 0 {
		best = enc
	}
	if len(best) == 0 {
		return nil, "", false
	}
	return best, "jpeg", true
}

// fitLongEdge returns the dimensions of w×h scaled so its longest edge is at most
// edge, never upscaling.
func fitLongEdge(w, h, edge int) (int, int) {
	if w <= edge && h <= edge {
		return w, h
	}
	if w >= h {
		return edge, max(h*edge/w, 1)
	}
	return max(w*edge/h, 1), edge
}

// resizeImage scales src to w×h using a high-quality kernel.
func resizeImage(src image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// encodeJPEG encodes img to JPEG at the given quality.
func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
