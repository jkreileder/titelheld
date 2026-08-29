package naming

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// response is the JSON shape every provider is asked for.
type response struct {
	Title    string `json:"title"`
	Language string `json:"language"`
}

// ParseAndValidate turns a raw model response into a title, or explains why it
// is not one. It also reports whatever followed the JSON, so a caller can log
// what the model actually sent.
//
// The contract is one JSON object and nothing else, and providers deviate from
// it in two shapes that carry no meaning. One is a markdown fence around the
// whole body. The other is trailing bytes after a complete object — Gemini
// returned well-formed JSON followed by a stray "}" on two of three calls on
// 2026-08-29, and each rejection failed the activity, left it queued and spent
// another five minutes and another call before a response happened to parse.
// Strictness about a byte after the object protects nothing, and the re-roll it
// forces changes which title lands: the title written is the first that parses,
// which is a sampling loop nobody chose.
//
// So the first JSON value is decoded and the schema is enforced strictly after
// it — the leniency is about where the object ends, never about what is in it.
// Anything that is not a JSON object at all still fails, and the raw text
// travels in the error so a caller can log what came back rather than a guess
// about it.
func (v Validator) ParseAndValidate(raw string) (Title, string, error) {
	body := unfence(strings.TrimSpace(raw))

	if body == "" {
		return Title{}, "", ErrNoTitle
	}

	var parsed response

	reader := strings.NewReader(body)

	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&parsed); err != nil {
		return Title{}, "", fmt.Errorf("naming: response is not JSON: %w (response: %q)", err, truncate(raw))
	}

	title, err := v.Validate(parsed.Title, Language(strings.ToLower(strings.TrimSpace(parsed.Language))))

	return title, trailing(decoder, reader), err
}

// trailing is whatever followed the decoded value, trimmed and bounded.
//
// Both halves are needed. The decoder buffers ahead, so some of the remainder
// is already inside it and the rest is still in the reader — asking either one
// alone truncates the evidence at whatever chunk boundary the decoder happened
// to stop at.
func trailing(decoder *json.Decoder, rest io.Reader) string {
	remainder, err := io.ReadAll(io.MultiReader(decoder.Buffered(), rest))
	if err != nil {
		return ""
	}

	return truncate(strings.TrimSpace(string(remainder)))
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
