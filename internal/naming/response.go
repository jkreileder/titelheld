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
// is not one. It also reports whatever followed the JSON, so a caller can log
// what the model actually sent.
//
// The contract is one JSON object and nothing else, and providers deviate from
// it in two shapes that carry no meaning. One is a markdown fence around the
// whole body. The other is trailing bytes after a complete object — a stray
// closing brace, a second object, a sentence. Strictness about either protects
// nothing: what follows a complete object says nothing about the title inside
// it, and rejecting the response costs a re-roll that decides which title
// lands, since the one written is the first that parses.
//
// So the first JSON value is decoded and the schema is enforced strictly after
// it — the leniency is about where the object ends, never about what is in it.
// Anything that is not a JSON object at all still fails, and the raw text
// travels in the error so a caller can log what came back rather than a guess
// about it.
func (v Validator) ParseAndValidate(raw string) (Title, string, error) {
	body, afterFence := unfence(strings.TrimSpace(raw))

	if body == "" {
		return Title{}, "", ErrNoTitle
	}

	var parsed response

	decoder := json.NewDecoder(strings.NewReader(body))
	if err := decoder.Decode(&parsed); err != nil {
		return Title{}, "", fmt.Errorf("naming: response is not JSON: %w (response: %q)", err, truncate(raw))
	}

	title, err := v.Validate(parsed.Title, Language(strings.ToLower(strings.TrimSpace(parsed.Language))))

	return title, trailing(body, decoder, afterFence), err
}

// trailing is everything the response carried besides the JSON value.
//
// Two disjoint regions, because a fenced response has two places to put
// something: after the value but inside the fence, and after the fence itself.
// Both are the same signal — a provider that keeps talking past the object —
// and dropping either would make the caller's warning a partial truth.
//
// The part inside the fence is taken by offset rather than by reading the
// decoder out: InputOffset is where the value ended, while the decoder's own
// buffer holds only as much as it happened to read ahead.
func trailing(body string, decoder *json.Decoder, afterFence string) string {
	inside := strings.TrimSpace(body[decoder.InputOffset():])

	return truncate(strings.TrimSpace(inside + "\n" + afterFence))
}

// unfence strips a markdown code fence, and hands back whatever followed it.
//
// The suffix is returned rather than discarded because it is evidence: a model
// that closes the fence and then explains itself has deviated from the
// contract twice, and only one of those is unambiguous enough to forgive
// silently.
func unfence(body string) (inside, after string) {
	if !strings.HasPrefix(body, "```") {
		return body, ""
	}

	body = strings.TrimPrefix(body, "```")

	// A fence may name its language: ```json
	if newline := strings.IndexByte(body, '\n'); newline >= 0 {
		if first := strings.TrimSpace(body[:newline]); !strings.Contains(first, "{") {
			body = body[newline+1:]
		}
	}

	if before, rest, ok := strings.CutLast(body, "```"); ok {
		return strings.TrimSpace(before), strings.TrimSpace(rest)
	}

	return strings.TrimSpace(body), ""
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
