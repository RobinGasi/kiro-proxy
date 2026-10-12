package converter

import (
	"encoding/json"
	"strings"
)

var effortOrder = []string{"low", "medium", "high", "xhigh", "max"}

// buildNativeReasoningFields builds only fields explicitly advertised by Kiro's
// live additionalModelRequestFieldsSchema. Unknown or malformed schemas fall
// back to the existing emulated-thinking path.
func buildNativeReasoningFields(raw []byte, thinking ThinkingConfig) map[string]any {
	if !thinking.Enabled || len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}

	field := ""
	var allowed []string
	for _, candidate := range []string{"reasoning", "output_config"} {
		if property, ok := schema.Properties[candidate]; ok {
			if effort, ok := property.Properties["effort"]; ok && len(effort.Enum) > 0 {
				field, allowed = candidate, effort.Enum
				break
			}
		}
	}
	if field == "" {
		return nil
	}
	effort := nearestEffort(thinking.Effort, allowed)
	if effort == "" {
		return nil
	}
	result := map[string]any{field: map[string]any{"effort": effort}}
	if field == "output_config" {
		adaptive := map[string]any{"type": "adaptive"}
		if thinkingProperty, ok := schema.Properties["thinking"]; ok {
			if display, ok := thinkingProperty.Properties["display"]; ok && contains(display.Enum, "summarized") {
				adaptive["display"] = "summarized"
			}
		}
		result["thinking"] = adaptive
	}
	return result
}

// buildGenerationFields copies only client controls explicitly advertised as
// top-level Kiro additionalModelRequestFields properties.
func buildGenerationFields(raw []byte, generation GenerationConfig) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}
	fields := make(map[string]any)
	copyIfAdvertised := func(names []string, value any) {
		if value == nil {
			return
		}
		for _, name := range names {
			if _, ok := schema.Properties[name]; ok {
				fields[name] = value
				return
			}
		}
	}
	if generation.MaxTokens != nil {
		copyIfAdvertised([]string{"max_tokens", "maxTokens", "max_output_tokens", "maxOutputTokens"}, *generation.MaxTokens)
	}
	if generation.Temperature != nil {
		copyIfAdvertised([]string{"temperature"}, *generation.Temperature)
	}
	if generation.TopP != nil {
		copyIfAdvertised([]string{"top_p", "topP"}, *generation.TopP)
	}
	if generation.TopK != nil {
		copyIfAdvertised([]string{"top_k", "topK"}, *generation.TopK)
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func mergeRequestFields(groups ...map[string]any) map[string]any {
	merged := make(map[string]any)
	for _, group := range groups {
		for key, value := range group {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func nearestEffort(requested string, allowed []string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "minimal" || requested == "" {
		requested = "low"
	}
	if contains(allowed, requested) {
		return requested
	}
	index := 0
	for i, effort := range effortOrder {
		if effort == requested {
			index = i
			break
		}
	}
	for distance := 1; distance < len(effortOrder); distance++ {
		if high := index + distance; high < len(effortOrder) && contains(allowed, effortOrder[high]) {
			return effortOrder[high]
		}
		if low := index - distance; low >= 0 && contains(allowed, effortOrder[low]) {
			return effortOrder[low]
		}
	}
	return ""
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
