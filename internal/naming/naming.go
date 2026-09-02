// Package naming turns everything known about a ride into a title.
//
// It holds the prompt builder, the validator every candidate title must pass,
// and the [Provider] interface the LLM implementations satisfy.
//
// The boundary runs inside the package, not around it. Nothing that decides a
// title touches a network: [BuildPrompt] and [Validator] work on values, and a
// caller supplies the provider, so the whole pipeline can be exercised with no
// network at all. The two shipped transports live here too and do import
// net/http — the same shape as geo, where the Describer is pure and Nominatim
// is not, and strava, where the client sits beside the types it returns.
//
// The division of labor between the prompt and the validator is deliberate.
// The prompt asks for a title of a certain shape; the validator decides whether
// what came back is one. Instructions to a model are a request, not a
// guarantee, so nothing downstream trusts them — and the same rule covers
// prompt injection, since the ride description is attacker-influenced text
// that reaches the model as data.
package naming

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// MaxTitleRunes is the longest title this service will accept.
//
// Counted in runes rather than bytes: the titles are frequently German, and a
// limit that counted bytes would silently be stricter for "Höhenmeter" than for
// "Hills".
const MaxTitleRunes = 60

// Language is the language a title is written in.
type Language string

// The only languages this service produces: German for utility and local
// rides, English where the ride suggests it. The model picks per ride.
const (
	German  Language = "de"
	English Language = "en"
)

// Valid reports whether the language is one this service accepts.
func (l Language) Valid() bool {
	return l == German || l == English
}

// Title is a validated title. Construction is not enough to trust one — only
// [Validator.Validate] returns it.
type Title struct {
	Text     string
	Language Language
}

// Provider is one LLM behind one method. The naming layer defines it, and the
// implementations in this package satisfy it; nothing here imports a vendor
// SDK.
type Provider interface {
	// Complete sends the prompt and returns the raw model response. Parsing
	// and validation are the caller's job, so a provider stays a transport.
	Complete(ctx context.Context, prompt Prompt) (string, error)

	// Name identifies the provider in logs.
	Name() string
}

// Prompt is what a provider sends: a system instruction and the ride itself.
// Splitting them lets a provider map them onto whatever its API calls the two
// roles, without the prompt builder knowing the difference.
type Prompt struct {
	System string
	User   string
}

// Validation failures. They are distinguished because the caller's response
// differs: a malformed response may be worth one retry, a banned word means
// the prompt needs work, and an over-long title is the most common failure and
// worth counting separately.
var (
	// ErrNoTitle means the model returned nothing usable.
	ErrNoTitle = errors.New("naming: the model returned no title")

	// ErrTitleTooLong means the title exceeds [MaxTitleRunes].
	ErrTitleTooLong = errors.New("naming: title is too long")

	// ErrTitleBanned means the title contains a configured banned word.
	ErrTitleBanned = errors.New("naming: title contains a banned word")

	// ErrTitleShape means the title carries characters a title must not:
	// emoji, quotation marks, or line breaks.
	ErrTitleShape = errors.New("naming: title has a disallowed shape")

	// ErrBadLanguage means the model reported a language this service does
	// not produce.
	ErrBadLanguage = errors.New("naming: unsupported language")

	// ErrTitleClaimsEntry means the title claims a franchise entry that is
	// not this service's to spend: one the athlete reserved, one the rotation
	// has already spent, or one it has yet to offer.
	ErrTitleClaimsEntry = errors.New("naming: title claims a franchise entry")

	// ErrTitleCopiesSegment means the title is one of the ride's achievement
	// names taken whole — somebody else's words as the entire title.
	ErrTitleCopiesSegment = errors.New("naming: title copies a segment name")
)

// DefaultBannedWords is the list the spec ships with. It is configuration, not
// code: an athlete's per-profile document replaces it.
func DefaultBannedWords() []string {
	return []string{"Epic", "Crushing", "Beast"}
}

// Validator decides whether a candidate title may be used.
//
// This is the enforcement point. Every constraint the prompt states is checked
// again here, because the prompt cannot bind the model and because the ride
// description that reaches the model is text this service did not write.
type Validator struct {
	banned []string
}

// NewValidator builds a validator.
//
// Banned words match case-insensitively and as substrings, so "Epic" also
// rejects "Epically" — and, less obviously, any word that merely contains one:
// a banned "ass" would reject "Musterpass". Choose entries that are safe to match
// anywhere, or the list will reject titles nobody objected to.
func NewValidator(banned []string) Validator {
	folded := make([]string, 0, len(banned))

	for _, word := range banned {
		trimmed := strings.TrimSpace(word)
		if trimmed == "" {
			continue
		}

		folded = append(folded, strings.ToLower(trimmed))
	}

	return Validator{banned: folded}
}

// Validate checks a candidate and returns the title it may be used as.
//
// Whitespace is normalized first: a model that returns a padded or
// internally-double-spaced title has not made a mistake worth failing over, and
// normalizing before measuring keeps the length limit honest.
func (v Validator) Validate(candidate string, language Language) (Title, error) {
	text := strings.Join(strings.Fields(candidate), " ")

	if text == "" {
		return Title{}, ErrNoTitle
	}

	if !language.Valid() {
		return Title{}, fmt.Errorf("%w: %q", ErrBadLanguage, language)
	}

	if count := len([]rune(text)); count > MaxTitleRunes {
		return Title{}, fmt.Errorf("%w: %d runes, limit %d", ErrTitleTooLong, count, MaxTitleRunes)
	}

	if err := checkShape(text); err != nil {
		return Title{}, err
	}

	lowered := strings.ToLower(text)
	for _, word := range v.banned {
		if strings.Contains(lowered, word) {
			return Title{}, fmt.Errorf("%w: %q", ErrTitleBanned, word)
		}
	}

	return Title{Text: text, Language: language}, nil
}

// checkShape rejects characters that have no place in a title.
//
// Quotation marks go because a model asked for JSON frequently returns the
// title still wrapped in them, and a title that arrives quoted would be written
// to Strava quoted. Emoji and other symbols go because the spec says so.
func checkShape(text string) error {
	for _, r := range text {
		switch {
		case r == '"' || r == '\'' || r == '“' || r == '”' || r == '„' || r == '‘' || r == '’':
			return fmt.Errorf("%w: quotation mark %q", ErrTitleShape, string(r))
		case unicode.IsControl(r):
			return fmt.Errorf("%w: control character", ErrTitleShape)
		case isEmojiPart(r):
			return fmt.Errorf("%w: emoji or symbol %q", ErrTitleShape, string(r))
		}
	}

	return nil
}

// isEmojiPart reports whether the rune is an emoji or a piece used to build
// one.
//
// So alone is not enough. An emoji is frequently a sequence rather than a
// character: "1️⃣" is a digit, a variation selector and a keycap, and a
// skin-toned or joined emoji adds a modifier or a zero-width joiner. Each of
// those pieces sits in a different Unicode category, and a title made of them
// passed a check that looked only at So.
//
// The pieces are named individually rather than by category, because the
// categories they belong to also hold things a German title legitimately uses:
// Mn holds the combining diaeresis that makes a decomposed "ö", so rejecting
// Mn wholesale would reject the language this service mostly writes in.
func isEmojiPart(r rune) bool {
	switch r {
	case '\uFE0F', // variation selector-16, which makes a character an emoji
		'\uFE0E', // variation selector-15, its text counterpart
		'\u200D', // zero width joiner, which builds compound emoji
		'\u20E3': // combining enclosing keycap
		return true
	}

	// Skin-tone modifiers.
	if r >= '\U0001F3FB' && r <= '\U0001F3FF' {
		return true
	}

	// The degree sign is category So, alongside the pictographs, and a title
	// about weather wants it: "5° und sonnig" is a title this athlete would
	// write. Carved out by hand because So is otherwise exactly the right
	// category to reject.
	if r == '°' {
		return false
	}

	// Pictographic symbols. Sk would also catch the modifiers above, but it
	// holds ordinary characters like the circumflex accent too, so only the
	// modifier block is taken from it.
	return r > unicode.MaxASCII && unicode.Is(unicode.So, r)
}
