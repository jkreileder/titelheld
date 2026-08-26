package naming

import (
	"encoding/json"
	"fmt"
	"strings"
)

// response is the JSON shape every provider is asked for.
type response struct {
	Title    string `json:"title"`
	Language string `json:"language"`
}

// ParseAndValidate turns a raw model response into a title, or explains why it
// is not one.
//
// Both providers are asked for bare JSON, and both are capable of returning it
// wrapped in a markdown fence anyway. Unwrapping that is not leniency about the
// contract — it is recognizing the one deviation that is unambiguous. Anything
// else fails, and the raw text travels in the error so a caller can log what
// actually came back rather than a guess about it.
func (v Validator) ParseAndValidate(raw string) (Title, error) {
	body := unfence(strings.TrimSpace(raw))

	if body == "" {
		return Title{}, ErrNoTitle
	}

	var parsed response
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return Title{}, fmt.Errorf("naming: response is not JSON: %w (response: %q)", err, truncate(raw))
	}

	return v.Validate(parsed.Title, Language(strings.ToLower(strings.TrimSpace(parsed.Language))))
}

// unfence strips a markdown code fence if the whole body is wrapped in one.
func unfence(body string) string {
	if !strings.HasPrefix(body, "```") {
		return body
	}

	body = strings.TrimPrefix(body, "```")

	// A fence may name its language: ```json
	if newline := strings.IndexByte(body, '\n'); newline >= 0 {
		if first := strings.TrimSpace(body[:newline]); !strings.Contains(first, "{") {
			body = body[newline+1:]
		}
	}

	if before, _, ok := strings.CutLast(body, "```"); ok {
		body = before
	}

	return strings.TrimSpace(body)
}

// truncate bounds what an error carries. A provider that returns a page of
// HTML should not put a page of HTML in a log line.
func truncate(s string) string {
	const limit = 300

	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}

	return string(runes[:limit]) + "…"
}
