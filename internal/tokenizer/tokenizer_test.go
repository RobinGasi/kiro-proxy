package tokenizer

import "testing"

// pyJSONTokens must be behavior-identical to the original
// countTokens(pyJSON(v), false) it replaced — the inline separator tally has to
// match what spaceSeparators would have produced, including inside escaped
// strings and for multi-byte runes.
func TestPyJSONTokensMatchesSpacedCount(t *testing.T) {
	cases := []any{
		map[string]any{"a": "b", "n": float64(1.5), "arr": []any{"x", "y"}},
		map[string]any{"nested": map[string]any{"k": "v", "deep": []any{float64(1), float64(2)}}},
		// commas/colons INSIDE a string must not be counted as separators.
		map[string]any{"s": "a,b:c", "t": "x"},
		// escaped quote then a structural separator after the string ends.
		map[string]any{"q": "he said \"hi\"", "after": float64(1)},
		// backslash escaping.
		map[string]any{"p": "C:\\Users\\x", "y": float64(2)},
		// multi-byte runes counted as code points.
		map[string]any{"u": "café, naïve: ok ☃", "z": "y"},
		map[string]any{},
		[]any{},
		"plain string",
		float64(42),
		nil,
	}
	for _, v := range cases {
		want := countTokens(pyJSON(v), false)
		got := pyJSONTokens(v)
		if got != want {
			t.Errorf("pyJSONTokens(%#v) = %d, want %d (pyJSON=%q)", v, got, want, pyJSON(v))
		}
	}
}
