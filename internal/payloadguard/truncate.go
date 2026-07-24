package payloadguard

// shrinkImagesToFit repeatedly downscales the single largest image in the
// payload until the serialized request fits maxBytes or no further image
// reduction is possible.
//
// It is non-destructive: images stay in the request, just re-encoded smaller
// (see shrinkLargestImage), so surrounding conversation context is untouched.
// For that reason it runs regardless of the auto-trim setting — an oversized
// screenshot should never cost the model its history. It returns true if any
// image was shrunk. The 32-iteration cap is a safety bound; shrinkLargestImage
// returning false (no progress) ends the loop earlier in practice.
func shrinkImagesToFit(payload map[string]any, maxBytes int) (shrunk bool, size int, err error) {
	for range 32 {
		size, err = CheckPayloadSize(payload)
		if err != nil {
			return shrunk, size, err
		}
		if size <= maxBytes {
			return shrunk, size, nil
		}
		if !shrinkLargestImage(payload, size-maxBytes) {
			return shrunk, size, nil
		}
		shrunk = true
	}
	// Iteration cap hit: the last measured size predates the final shrink, so
	// re-measure once for an accurate result. This path is a rare safety bound.
	size, err = CheckPayloadSize(payload)
	return shrunk, size, err
}
