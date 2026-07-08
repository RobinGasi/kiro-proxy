package converter

// SanitizeToolSchema removes the JSON Schema fields that the Kiro API rejects.
//
// Kiro returns a 400 "Improperly formed request." when a schema contains an empty
// "required": [] array or an "additionalProperties" key, so those are dropped.
// The schema is processed recursively (into "properties" values, nested objects,
// and list members such as anyOf/oneOf) and a sanitized copy is returned; the
// input is not modified.
func SanitizeToolSchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{}
	}

	result := make(map[string]any, len(schema))
	for key, value := range schema {
		// Skip empty required arrays.
		if key == "required" {
			if arr, ok := value.([]any); ok && len(arr) == 0 {
				continue
			}
		}

		// Skip additionalProperties — Kiro does not support it.
		if key == "additionalProperties" {
			continue
		}

		switch key {
		case "properties":
			if props, ok := value.(map[string]any); ok {
				sanitized := make(map[string]any, len(props))
				for propName, propValue := range props {
					if pv, ok := propValue.(map[string]any); ok {
						sanitized[propName] = SanitizeToolSchema(pv)
					} else {
						sanitized[propName] = propValue
					}
				}
				result[key] = sanitized
				continue
			}
		}

		switch v := value.(type) {
		case map[string]any:
			result[key] = SanitizeToolSchema(v)
		case []any:
			out := make([]any, len(v))
			for i, item := range v {
				if im, ok := item.(map[string]any); ok {
					out[i] = SanitizeToolSchema(im)
				} else {
					out[i] = item
				}
			}
			result[key] = out
		default:
			result[key] = v
		}
	}

	return result
}
