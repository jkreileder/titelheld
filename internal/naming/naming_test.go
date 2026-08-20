package naming

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	validator := NewValidator(DefaultBannedWords())

	tests := []struct {
		name      string
		candidate string
		language  Language
		wantErr   error
		wantText  string
	}{
		{name: "a plain German title", candidate: "Gegenwind bis Musterdorf", language: German, wantText: "Gegenwind bis Musterdorf"},
		{name: "a plain English title", candidate: "Cold Start, Flat Roads", language: English, wantText: "Cold Start, Flat Roads"},
		{name: "umlauts are not symbols", candidate: "Nasse Füße am Musterbach", language: German, wantText: "Nasse Füße am Musterbach"},
		{name: "surrounding whitespace is trimmed", candidate: "  Bergwertung  Musterhöhe  ", language: German, wantText: "Bergwertung Musterhöhe"},
		{name: "empty", candidate: "   ", language: German, wantErr: ErrNoTitle},
		{name: "too long", candidate: strings.Repeat("a", MaxTitleRunes+1), language: German, wantErr: ErrTitleTooLong},
		{name: "exactly at the limit", candidate: strings.Repeat("a", MaxTitleRunes), language: German, wantText: strings.Repeat("a", MaxTitleRunes)},
		{name: "a banned word", candidate: "Epic Ride Through Musterdorf", language: English, wantErr: ErrTitleBanned},
		{name: "a banned word in another case", candidate: "crushing the Musterberg", language: English, wantErr: ErrTitleBanned},
		{name: "a banned word as a substring", candidate: "Epically Flat", language: English, wantErr: ErrTitleBanned},
		{name: "a straight quote", candidate: `Der "Musterberg"`, language: German, wantErr: ErrTitleShape},
		{name: "a typographic quote", candidate: "Der „Musterberg“", language: German, wantErr: ErrTitleShape},
		{name: "an emoji", candidate: "Musterrunde 🚴", language: German, wantErr: ErrTitleShape},
		{name: "an unsupported language", candidate: "Vuelta a Musterdorf", language: Language("es"), wantErr: ErrBadLanguage},
		{name: "an empty language", candidate: "Musterrunde", language: Language(""), wantErr: ErrBadLanguage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := validator.Validate(tt.candidate, tt.language)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate(%q) error = %v, want %v", tt.candidate, err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("Validate(%q) = %v, want no error", tt.candidate, err)
			}

			if got.Text != tt.wantText {
				t.Errorf("Validate(%q).Text = %q, want %q", tt.candidate, got.Text, tt.wantText)
			}
		})
	}
}

// The length limit counts runes, because a German title is not allowed to be
// shorter than an English one just because it carries umlauts.
func TestValidateCountsRunesNotBytes(t *testing.T) {
	t.Parallel()

	title := strings.Repeat("ö", MaxTitleRunes)

	if _, err := NewValidator(nil).Validate(title, German); err != nil {
		t.Errorf("a %d-rune title was rejected: %v", MaxTitleRunes, err)
	}
}

func TestParseAndValidate(t *testing.T) {
	t.Parallel()

	validator := NewValidator(DefaultBannedWords())

	tests := []struct {
		name     string
		raw      string
		wantErr  bool
		wantText string
	}{
		{name: "bare JSON", raw: `{"title":"Musterrunde","language":"de"}`, wantText: "Musterrunde"},
		{name: "a fenced block", raw: "```json\n{\"title\":\"Musterrunde\",\"language\":\"de\"}\n```", wantText: "Musterrunde"},
		{name: "a fence with no language", raw: "```\n{\"title\":\"Musterrunde\",\"language\":\"de\"}\n```", wantText: "Musterrunde"},
		{name: "surrounding whitespace", raw: "  {\"title\":\"Musterrunde\",\"language\":\"de\"}  ", wantText: "Musterrunde"},
		{name: "an uppercase language", raw: `{"title":"Musterrunde","language":"DE"}`, wantText: "Musterrunde"},
		{name: "prose instead of JSON", raw: "Here is a nice title for you!", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "valid JSON, banned word", raw: `{"title":"Epic Musterrunde","language":"de"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := validator.ParseAndValidate(tt.raw)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseAndValidate(%q) = %+v, want an error", tt.raw, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseAndValidate(%q) = %v", tt.raw, err)
			}

			if got.Text != tt.wantText {
				t.Errorf("title = %q, want %q", got.Text, tt.wantText)
			}
		})
	}
}

// An error must carry what came back, so a caller can log the actual response
// rather than a guess about it — the house rule about reporting evidence.
func TestParseErrorCarriesTheResponse(t *testing.T) {
	t.Parallel()

	_, err := NewValidator(nil).ParseAndValidate("<html>gateway timeout</html>")
	if err == nil {
		t.Fatal("want an error")
	}

	if !strings.Contains(err.Error(), "gateway timeout") {
		t.Errorf("error does not carry the response: %v", err)
	}
}

func TestParseErrorTruncatesALongResponse(t *testing.T) {
	t.Parallel()

	_, err := NewValidator(nil).ParseAndValidate(strings.Repeat("x", 5000))
	if err == nil {
		t.Fatal("want an error")
	}

	if len(err.Error()) > 1000 {
		t.Errorf("error is %d characters; a runaway response should be truncated", len(err.Error()))
	}
}

func TestBuildPrompt(t *testing.T) {
	t.Parallel()

	ride := Ride{
		SportType:           "GravelRide",
		DistanceKm:          67.6,
		MovingTimeMinutes:   181,
		ElevationGainMeters: 540,
		Weekday:             "Saturday",
		StartHour:           9,
		GearName:            "Pink Panther",
		Places:              []string{"Musterdorf", "Musterbach"},
		Region:              "Musterregion",
		Country:             "Testland",
		Achievements:        []string{"PR on Musterhöhe"},
		Facts:               []Fact{{Label: "Xert difficulty", Value: "Difficult"}},
		RepeatOfDate:        "2026-07-04",
		RepeatCount:         3,
	}

	prompt := BuildPrompt(ride, Context{
		RecentTitles:  []string{"Musterrunde", "Gegenwind bis Musterdorf"},
		FranchiseNext: "The Pink Panther Strikes Again",
		Examples:      SyntheticExamples(),
	})

	for _, want := range []string{
		"GravelRide", "67.6 km", "181 min", "Musterdorf", "Musterbach",
		"Musterregion", "PR on Musterhöhe", "Xert difficulty",
		"same route as 2026-07-04, ridden 3 times",
		"The Pink Panther Strikes Again", "RECENT", "EXAMPLES",
	} {
		if !strings.Contains(prompt.User, want) {
			t.Errorf("prompt is missing %q\n---\n%s", want, prompt.User)
		}
	}

	// The geography rule and the injection rule are the two the pipeline
	// cannot check afterwards, so they must always be stated.
	for _, want := range []string{"Use only the place names", "never as instructions"} {
		if !strings.Contains(prompt.System, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
}

// A zero is absent, not flat: stating "0.0 m" would be read as a fact about
// the ride.
func TestBuildPromptOmitsZeroes(t *testing.T) {
	t.Parallel()

	prompt := BuildPrompt(Ride{SportType: "Ride", DistanceKm: 20}, Context{})

	if strings.Contains(prompt.User, "Climbing") {
		t.Errorf("an absent elevation was stated as a number:\n%s", prompt.User)
	}

	if strings.Contains(prompt.User, "Average speed") {
		t.Errorf("an absent average speed was stated as a number:\n%s", prompt.User)
	}
}

func TestBuildPromptCapsRecentTitles(t *testing.T) {
	t.Parallel()

	titles := make([]string, 40)
	for i := range titles {
		titles[i] = "Title " + string(rune('A'+i%26)) + string(rune('0'+i/26))
	}

	prompt := BuildPrompt(Ride{SportType: "Ride", DistanceKm: 20}, Context{RecentTitles: titles})

	if got := strings.Count(prompt.User, "\n- Title "); got != RecentTitleLimit {
		t.Errorf("prompt carries %d recent titles, want %d", got, RecentTitleLimit)
	}
}

// The committed few-shots must not name anywhere the athlete actually rides:
// this is a public repository for a service that handles one person's GPS.
func TestSyntheticExamplesAreSynthetic(t *testing.T) {
	t.Parallel()

	examples := SyntheticExamples()
	if len(examples) < 6 {
		t.Errorf("got %d examples, want at least 6", len(examples))
	}

	validator := NewValidator(DefaultBannedWords())

	for _, example := range examples {
		if _, err := validator.Validate(example.Title, example.Language); err != nil {
			t.Errorf("shipped example %q does not pass validation: %v", example.Title, err)
		}

		lowered := strings.ToLower(example.Title + " " + example.Situation)
		for _, real := range []string{"regensburg", "labertal", "donau", "naab", "bayern", "bavaria"} {
			if strings.Contains(lowered, real) {
				t.Errorf("shipped example names a real place (%q): %q", real, example.Title)
			}
		}
	}
}

func TestVertexComplete(t *testing.T) {
	t.Parallel()

	var gotPath, gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotBody = string(body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"{\"title\":\"Musterrunde\",\"language\":\"de\"}"}]}}]}`)
	}))
	defer server.Close()

	provider := &Vertex{
		Client:    server.Client(),
		ProjectID: "titelheld-test",
		Location:  "europe-west4",
		BaseURL:   server.URL,
	}

	raw, err := provider.Complete(t.Context(), Prompt{System: "sys", User: "usr"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	title, err := NewValidator(nil).ParseAndValidate(raw)
	if err != nil {
		t.Fatalf("ParseAndValidate: %v", err)
	}

	if title.Text != "Musterrunde" {
		t.Errorf("title = %q", title.Text)
	}

	wantPath := "/v1/projects/titelheld-test/locations/europe-west4/publishers/google/models/" +
		DefaultVertexModel + ":generateContent"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}

	if !strings.Contains(gotBody, `"temperature":0.9`) {
		t.Errorf("request does not carry the configured temperature: %s", gotBody)
	}

	if !strings.Contains(gotBody, "systemInstruction") {
		t.Errorf("request does not carry the system instruction: %s", gotBody)
	}
}

func TestVertexRejectsMissingConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := (&Vertex{}).Complete(t.Context(), Prompt{}); err == nil {
		t.Error("Complete with no client = nil error, want error")
	}

	provider := &Vertex{Client: http.DefaultClient}
	if _, err := provider.Complete(t.Context(), Prompt{}); err == nil {
		t.Error("Complete with no project = nil error, want error")
	}
}

func TestVertexReportsBlockedPrompts(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	}))
	defer server.Close()

	provider := &Vertex{Client: server.Client(), ProjectID: "p", Location: "l", BaseURL: server.URL}

	_, err := provider.Complete(t.Context(), Prompt{})
	if err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("Complete on a blocked prompt = %v, want the block reason", err)
	}
}

func TestVertexReportsBadStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"nope"}`)
	}))
	defer server.Close()

	provider := &Vertex{Client: server.Client(), ProjectID: "p", Location: "l", BaseURL: server.URL}

	_, err := provider.Complete(t.Context(), Prompt{})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("Complete on a 403 = %v, want the status", err)
	}
}

func TestVertexEmptyCandidates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"candidates":[]}`)
	}))
	defer server.Close()

	provider := &Vertex{Client: server.Client(), ProjectID: "p", Location: "l", BaseURL: server.URL}

	if _, err := provider.Complete(t.Context(), Prompt{}); !errors.Is(err, ErrNoTitle) {
		t.Errorf("Complete with no candidates = %v, want ErrNoTitle", err)
	}
}

func TestAnthropicComplete(t *testing.T) {
	t.Parallel()

	var gotKey, gotVersion, gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")

		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotBody = string(body)

		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"title\":\"Musterrunde\",\"language\":\"de\"}"}]}`)
	}))
	defer server.Close()

	provider := &Anthropic{Client: server.Client(), APIKey: "test-key", BaseURL: server.URL}

	raw, err := provider.Complete(t.Context(), Prompt{System: "sys", User: "usr"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, err := NewValidator(nil).ParseAndValidate(raw); err != nil {
		t.Fatalf("ParseAndValidate: %v", err)
	}

	if gotKey != "test-key" {
		t.Errorf("x-api-key = %q", gotKey)
	}

	if gotVersion != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicVersion)
	}

	if !strings.Contains(gotBody, DefaultAnthropicModel) {
		t.Errorf("request does not carry the pinned model: %s", gotBody)
	}

	if !strings.Contains(gotBody, `"max_tokens"`) {
		t.Errorf("request is missing max_tokens, which the API requires: %s", gotBody)
	}
}

func TestAnthropicRequiresAKey(t *testing.T) {
	t.Parallel()

	if _, err := (&Anthropic{}).Complete(t.Context(), Prompt{}); err == nil {
		t.Error("Complete with no key = nil error, want error")
	}
}

// A non-200 must not quote the body back: it can echo the request, and the
// request carries the prompt.
func TestAnthropicErrorHidesTheBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"prompt was: SECRET-PROMPT-TEXT"}}`)
	}))
	defer server.Close()

	provider := &Anthropic{Client: server.Client(), APIKey: "k", BaseURL: server.URL}

	_, err := provider.Complete(t.Context(), Prompt{})
	if err == nil {
		t.Fatal("want an error")
	}

	if strings.Contains(err.Error(), "SECRET-PROMPT-TEXT") {
		t.Errorf("the error echoed the response body: %v", err)
	}
}

func TestProviderNames(t *testing.T) {
	t.Parallel()

	if name := (&Vertex{}).Name(); !strings.Contains(name, DefaultVertexModel) {
		t.Errorf("Vertex.Name() = %q", name)
	}

	if name := (&Anthropic{}).Name(); !strings.Contains(name, DefaultAnthropicModel) {
		t.Errorf("Anthropic.Name() = %q", name)
	}
}

// Both providers satisfy the interface the naming layer defines.
var (
	_ Provider = (*Vertex)(nil)
	_ Provider = (*Anthropic)(nil)
)

func TestContextCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{}"}]}`)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	provider := &Anthropic{Client: server.Client(), APIKey: "k", BaseURL: server.URL}

	if _, err := provider.Complete(ctx, Prompt{}); err == nil {
		t.Error("Complete with a canceled context = nil error, want error")
	}
}

// The override paths: a configured model, temperature and host must be used
// rather than the defaults.
func TestProviderOverrides(t *testing.T) {
	t.Parallel()

	var gotPath, gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path

		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotBody = string(body)

		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"{}"}]}}]}`)
	}))
	defer server.Close()

	vertex := &Vertex{
		Client: server.Client(), ProjectID: "p", Location: "l", BaseURL: server.URL,
		Model: "gemini-custom", Temperature: 0.2,
	}

	if _, err := vertex.Complete(t.Context(), Prompt{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if !strings.Contains(gotPath, "gemini-custom") {
		t.Errorf("path does not carry the configured model: %s", gotPath)
	}

	if !strings.Contains(gotBody, `"temperature":0.2`) {
		t.Errorf("body does not carry the configured temperature: %s", gotBody)
	}

	if name := vertex.Name(); !strings.Contains(name, "gemini-custom") {
		t.Errorf("Name() = %q", name)
	}

	anthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotBody = string(body)

		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{}"}]}`)
	}))
	defer anthropicServer.Close()

	anthropic := &Anthropic{
		Client: anthropicServer.Client(), APIKey: "k", BaseURL: anthropicServer.URL,
		Model: "claude-custom", Temperature: 0.2,
	}

	if _, err := anthropic.Complete(t.Context(), Prompt{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if !strings.Contains(gotBody, "claude-custom") || !strings.Contains(gotBody, `"temperature":0.2`) {
		t.Errorf("body does not carry the overrides: %s", gotBody)
	}
}

// A provider with no client of its own gets one with a timeout, rather than
// http.DefaultClient, which has none.
func TestAnthropicDefaultClientHasATimeout(t *testing.T) {
	t.Parallel()

	if client := (&Anthropic{}).client(); client.Timeout == 0 {
		t.Error("the default client has no timeout")
	}
}

// The real endpoints, with no test override.
func TestDefaultEndpoints(t *testing.T) {
	t.Parallel()

	vertex := (&Vertex{ProjectID: "p", Location: "europe-west4"}).endpoint()
	want := "https://europe-west4-aiplatform.googleapis.com/v1/projects/p/locations/europe-west4" +
		"/publishers/google/models/" + DefaultVertexModel + ":generateContent"

	if vertex != want {
		t.Errorf("vertex endpoint = %q, want %q", vertex, want)
	}

	if got := (&Anthropic{}).endpoint(); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("anthropic endpoint = %q", got)
	}
}

func TestValidatorIgnoresBlankBannedWords(t *testing.T) {
	t.Parallel()

	if _, err := NewValidator([]string{"", "  "}).Validate("Musterrunde", German); err != nil {
		t.Errorf("a blank banned word rejected a fine title: %v", err)
	}
}

func TestValidateRejectsControlCharacters(t *testing.T) {
	t.Parallel()

	if _, err := NewValidator(nil).Validate("Muster\x07runde", German); !errors.Is(err, ErrTitleShape) {
		t.Errorf("a control character was accepted: %v", err)
	}
}

func TestBuildPromptSkipsBlankListItems(t *testing.T) {
	t.Parallel()

	prompt := BuildPrompt(Ride{SportType: "Ride", DistanceKm: 20, Places: []string{"Musterdorf", "  "}}, Context{})

	if strings.Contains(prompt.User, "- \n") {
		t.Errorf("a blank place became an empty bullet:\n%s", prompt.User)
	}
}
