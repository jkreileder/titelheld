package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// env builds a getenv function over a map, with every required variable set to
// a valid placeholder unless the caller overrides or clears it.
func env(overrides map[string]string) func(string) string {
	values := map[string]string{
		"STRAVA_CLIENT_ID":     "12345",
		"STRAVA_CLIENT_SECRET": "test-client-secret",
		"STRAVA_VERIFY_TOKEN":  "test-verify-token",
		"BASE_URL":             "https://namer.example.invalid",
		"WEBHOOK_PATH_SECRET":  "s3cr3t-segment",
		"MAX_INSTANCES":        "1",
	}

	for key, value := range overrides {
		values[key] = value
	}

	return func(key string) string { return values[key] }
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.ProcessDelay != DefaultProcessDelay {
		t.Errorf("ProcessDelay = %v, want %v", cfg.ProcessDelay, DefaultProcessDelay)
	}
	if cfg.WebhookPath != "/webhook/s3cr3t-segment" {
		t.Errorf("WebhookPath = %q", cfg.WebhookPath)
	}
	if cfg.AuthPath != "/auth/s3cr3t-segment" {
		t.Errorf("AuthPath = %q", cfg.AuthPath)
	}
	if cfg.RedirectURL() != "https://namer.example.invalid/auth/callback" {
		t.Errorf("RedirectURL() = %q", cfg.RedirectURL())
	}
	if cfg.AthleteID != 0 {
		t.Errorf("AthleteID = %d, want 0 when unset", cfg.AthleteID)
	}
}

// TestDryRunIsTheDefault is the constraint with teeth: nothing about an
// unconfigured environment may enable writes.
func TestDryRunIsTheDefault(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.WritesEnabled {
		t.Error("WritesEnabled = true with DRY_RUN unset, want false")
	}
	if !cfg.DryRun() {
		t.Error("DryRun() = false with DRY_RUN unset, want true")
	}
}

func TestZeroConfigIsDryRun(t *testing.T) {
	t.Parallel()

	if !(Config{}).DryRun() {
		t.Error("a zero Config must be dry run")
	}
}

func TestDryRunParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw               string
		wantWritesEnabled bool
		wantErr           bool
	}{
		{raw: "", wantWritesEnabled: false},
		{raw: "1", wantWritesEnabled: false},
		{raw: "true", wantWritesEnabled: false},
		{raw: "TRUE", wantWritesEnabled: false},
		{raw: " yes ", wantWritesEnabled: false},
		{raw: "on", wantWritesEnabled: false},
		{raw: "0", wantWritesEnabled: true},
		{raw: "false", wantWritesEnabled: true},
		{raw: "FALSE", wantWritesEnabled: true},
		{raw: "no", wantWritesEnabled: true},
		{raw: "off", wantWritesEnabled: true},
		// Anything unrecognized is an error and leaves writes disabled: a typo
		// must never be what lets this service write.
		{raw: "flase", wantWritesEnabled: false, wantErr: true},
		{raw: "2", wantWritesEnabled: false, wantErr: true},
		{raw: "disabled", wantWritesEnabled: false, wantErr: true},
	}

	for _, tt := range tests {
		t.Run("DRY_RUN="+tt.raw, func(t *testing.T) {
			t.Parallel()

			enabled, err := parseWritesEnabled(tt.raw)
			if enabled != tt.wantWritesEnabled {
				t.Errorf("writesEnabled = %v, want %v", enabled, tt.wantWritesEnabled)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}

			cfg, loadErr := Load(env(map[string]string{"DRY_RUN": tt.raw}))
			if tt.wantErr {
				if loadErr == nil {
					t.Fatal("Load = nil error, want the DRY_RUN error")
				}
				// A rejected config must not be usable at all.
				if cfg.WritesEnabled {
					t.Error("a failed Load returned a config with writes enabled")
				}

				return
			}

			if loadErr != nil {
				t.Fatalf("Load: %v", loadErr)
			}
			if cfg.WritesEnabled != tt.wantWritesEnabled {
				t.Errorf("cfg.WritesEnabled = %v, want %v", cfg.WritesEnabled, tt.wantWritesEnabled)
			}
		})
	}
}

func TestRequiredVariables(t *testing.T) {
	t.Parallel()

	required := []string{
		"STRAVA_CLIENT_ID",
		"STRAVA_CLIENT_SECRET",
		"STRAVA_VERIFY_TOKEN",
		"BASE_URL",
		"WEBHOOK_PATH_SECRET",
	}

	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(env(map[string]string{name: ""}))
			if err == nil {
				t.Fatalf("Load without %s = nil error, want error", name)
			}

			if missing, ok := errors.AsType[*ErrMissing](err); !ok || missing.Name != name {
				t.Errorf("error = %v, want an ErrMissing for %s", err, name)
			}
		})
	}
}

func TestErrorsNeverEchoSecretValues(t *testing.T) {
	t.Parallel()

	_, err := Load(env(map[string]string{
		"BASE_URL": "",
		"DRY_RUN":  "nonsense",
	}))
	if err == nil {
		t.Fatal("Load = nil error, want error")
	}

	for _, secret := range []string{"test-client-secret", "test-verify-token", "s3cr3t-segment"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaked a secret value: %v", err)
		}
	}
}

func TestAllMissingVariablesAreReportedTogether(t *testing.T) {
	t.Parallel()

	_, err := Load(env(map[string]string{
		"STRAVA_CLIENT_ID":    "",
		"STRAVA_VERIFY_TOKEN": "",
	}))
	if err == nil {
		t.Fatal("Load = nil error, want error")
	}

	for _, name := range []string{"STRAVA_CLIENT_ID", "STRAVA_VERIFY_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %v does not mention %s", err, name)
		}
	}
}

func TestOptionalOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{
		"PORT":              "9090",
		"PROCESS_DELAY":     "90s",
		"STRAVA_ATHLETE_ID": "4242",
		"BASE_URL":          "https://namer.example.invalid/",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != 9090 {
		t.Errorf("Port = %d", cfg.Port)
	}
	if cfg.ProcessDelay != 90*time.Second {
		t.Errorf("ProcessDelay = %v", cfg.ProcessDelay)
	}
	if cfg.AthleteID != 4242 {
		t.Errorf("AthleteID = %d", cfg.AthleteID)
	}
	if cfg.BaseURL != "https://namer.example.invalid" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", cfg.BaseURL)
	}
}

func TestWebhookPathSecretIsNormalized(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{"WEBHOOK_PATH_SECRET": "/wrapped-segment/"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.WebhookPath != "/webhook/wrapped-segment" {
		t.Errorf("WebhookPath = %q", cfg.WebhookPath)
	}
	if cfg.AuthPath != "/auth/wrapped-segment" {
		t.Errorf("AuthPath = %q", cfg.AuthPath)
	}
}

// A path secret that http.ServeMux would reject, or silently read as a
// wildcard, must be caught at load time rather than at route registration.
func TestPathSecretIsValidated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret string
	}{
		{name: "space would panic ServeMux", secret: "has space"},
		{name: "tab would panic ServeMux", secret: "has\ttab"},
		{name: "braces register a wildcard", secret: "{anything}"},
		{name: "slash splits the segment", secret: "two/segments"},
		{name: "too short to be unguessable", secret: "short"},
		{name: "collides with the OAuth callback", secret: "callback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := Load(env(map[string]string{"WEBHOOK_PATH_SECRET": tt.secret})); err == nil {
				t.Fatalf("Load with secret %q = nil error, want error", tt.secret)
			}
		})
	}
}

func TestInvalidOptionalValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		raw  string
	}{
		{name: "port not a number", key: "PORT", raw: "http"},
		{name: "port out of range", key: "PORT", raw: "70000"},
		{name: "port zero", key: "PORT", raw: "0"},
		{name: "athlete id not a number", key: "STRAVA_ATHLETE_ID", raw: "me"},
		{name: "athlete id negative", key: "STRAVA_ATHLETE_ID", raw: "-1"},
		{name: "delay not a duration", key: "PROCESS_DELAY", raw: "ten minutes"},
		{name: "delay negative", key: "PROCESS_DELAY", raw: "-5m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := Load(env(map[string]string{tt.key: tt.raw})); err == nil {
				t.Fatalf("Load with %s=%q = nil error, want error", tt.key, tt.raw)
			}
		})
	}
}

func TestErrMissingMessage(t *testing.T) {
	t.Parallel()

	err := &ErrMissing{Name: "SOME_VAR"}
	if !strings.Contains(err.Error(), "SOME_VAR") {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestFirestoreConfiguration(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Unset means the in-memory store, which loses the OAuth token on restart.
	if cfg.PersistentStore() {
		t.Error("PersistentStore() = true with FIRESTORE_PROJECT unset")
	}

	cfg, err = Load(env(map[string]string{
		"FIRESTORE_PROJECT":  "titelheld-prod",
		"FIRESTORE_DATABASE": "titelheld",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.PersistentStore() {
		t.Error("PersistentStore() = false with FIRESTORE_PROJECT set")
	}
	if cfg.FirestoreProject != "titelheld-prod" || cfg.FirestoreDatabase != "titelheld" {
		t.Errorf("firestore config = %q/%q", cfg.FirestoreProject, cfg.FirestoreDatabase)
	}
}

// A database named without a project would otherwise start cleanly on the
// in-memory store and drop the rotated refresh token at the first restart.
func TestFirestoreDatabaseWithoutProjectIsRejected(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{"FIRESTORE_DATABASE": "titelheld"}))
	if err == nil {
		t.Fatal("Load with a database but no project = nil error, want error")
	}

	if !strings.Contains(err.Error(), EnvFirestoreProject) {
		t.Errorf("error %v does not name %s", err, EnvFirestoreProject)
	}

	if cfg.PersistentStore() {
		t.Error("a rejected config reported a persistent store")
	}
}

func TestNominatimUserAgent(t *testing.T) {
	t.Parallel()

	// Nominatim blocks anonymous clients, so there is always an identity.
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.NominatimUserAgent != "titelheld/1.0 (+https://namer.example.invalid)" {
		t.Errorf("default User-Agent = %q", cfg.NominatimUserAgent)
	}

	cfg, err = Load(env(map[string]string{
		"NOMINATIM_USER_AGENT": "titelheld (jk@example.invalid)",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.NominatimUserAgent != "titelheld (jk@example.invalid)" {
		t.Errorf("User-Agent = %q, want the override", cfg.NominatimUserAgent)
	}
}

func TestLLMDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{"FIRESTORE_PROJECT": "titelheld-test"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.LLM.Provider != ProviderVertex {
		t.Errorf("provider = %q, want %q", cfg.LLM.Provider, ProviderVertex)
	}

	if cfg.LLM.VertexProject != "titelheld-test" {
		t.Errorf("vertex project = %q, want the Firestore project", cfg.LLM.VertexProject)
	}

	if cfg.LLM.VertexLocation != DefaultVertexLocation {
		t.Errorf("vertex location = %q, want %q", cfg.LLM.VertexLocation, DefaultVertexLocation)
	}

	// The default provider is keyless; nothing should have been read.
	if cfg.LLM.APIKey != "" {
		t.Errorf("an API key was loaded for the keyless provider: %q", cfg.LLM.APIKey)
	}
}

func TestLLMAnthropicRequiresAKey(t *testing.T) {
	t.Parallel()

	_, err := Load(env(map[string]string{"LLM_PROVIDER": "anthropic"}))
	if err == nil {
		t.Fatal("anthropic without a key = nil error, want error")
	}

	if !strings.Contains(err.Error(), EnvLLMAPIKey) {
		t.Errorf("error does not name the missing variable: %v", err)
	}
}

func TestLLMAnthropicWithAKey(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{"LLM_PROVIDER": "anthropic", "LLM_API_KEY": "k"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.LLM.Provider != ProviderAnthropic || cfg.LLM.APIKey != "k" {
		t.Errorf("llm = %+v", cfg.LLM)
	}
}

func TestLLMRejectsAnUnknownProvider(t *testing.T) {
	t.Parallel()

	if _, err := Load(env(map[string]string{"LLM_PROVIDER": "openai"})); err == nil {
		t.Error("an unknown provider = nil error, want error")
	}
}

// Machine-title patterns are newline-separated because they are regular
// expressions, and a comma inside one is ordinary rather than a separator.
func TestLLMListsAreParsed(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{
		"BANNED_WORDS":           "Epic, Crushing ,, Beast",
		"MACHINE_TITLE_PATTERNS": "^A{1,3} Ride$\n\n^B Ride$",
		"VERTEX_LOCATION":        "europe-west3",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := len(cfg.LLM.BannedWords); got != 3 {
		t.Errorf("banned words = %v, want 3 entries", cfg.LLM.BannedWords)
	}

	if got := cfg.LLM.MachineTitlePatterns; len(got) != 2 || got[0] != "^A{1,3} Ride$" {
		t.Errorf("machine-title patterns = %q, want the comma inside the pattern preserved", got)
	}

	if cfg.LLM.VertexLocation != "europe-west3" {
		t.Errorf("vertex location = %q", cfg.LLM.VertexLocation)
	}
}

// VERTEX_LOCATION is interpolated into the request host, so a malformed value
// would send the runtime account's credentials somewhere this deployment never
// meant to reach.
func TestLLMRejectsAMalformedVertexLocation(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"evil.example/x", "europe west3", "EUROPE-WEST3", "-west3", ""} {
		vars := map[string]string{"VERTEX_LOCATION": bad}
		if bad == "" {
			continue // empty falls back to the default, which is covered elsewhere
		}

		if _, err := Load(env(vars)); err == nil {
			t.Errorf("VERTEX_LOCATION=%q was accepted", bad)
		}
	}

	for _, good := range []string{"europe-west3", "europe-west4", "us-central1", "global"} {
		if _, err := Load(env(map[string]string{"VERTEX_LOCATION": good})); err != nil {
			t.Errorf("VERTEX_LOCATION=%q was rejected: %v", good, err)
		}
	}
}

// The health route must not drift back onto a path the platform answers.
//
// Cloud Run's frontend serves "/healthz" itself: a request for it never
// reaches the container, and the route was unreachable in production from the
// first deploy without anything failing. Every other test here routes through
// the constant, so moving it back would pass the whole suite — this is the one
// assertion that would not.
func TestHealthPathAvoidsThePlatformsReservedPath(t *testing.T) {
	t.Parallel()

	if HealthPath == "/healthz" {
		t.Fatal("HealthPath is /healthz, which Cloud Run's frontend answers before the container")
	}

	if HealthPath != "/health" {
		t.Errorf("HealthPath = %q; if this moved deliberately, move this assertion with it",
			HealthPath)
	}
}

// The service refuses to start unless Cloud Run told it there is one instance.
//
// Four pieces of state in this process — the OAuth state map, the first-bind
// lock, token-refresh serialization and the sweep lock — are correct only
// because there is one container. Terraform sets max_instance_count = 1 and
// passes the same number in as MAX_INSTANCES; this is the binary checking that
// it was told, rather than assuming.
func TestMaxInstancesIsRequiredToServe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "unset", raw: "", want: "is not set"},
		{name: "two", raw: "2", want: `"2"`},
		{name: "zero", raw: "0", want: `"0"`},
		{name: "empty-ish", raw: "  ", want: "is not set"},
		{name: "not a number", raw: "one", want: `"one"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(env(map[string]string{"MAX_INSTANCES": tc.raw}))
			if err == nil {
				t.Fatalf("MAX_INSTANCES=%q was accepted", tc.raw)
			}

			// The evidence, not an interpretation of it: an unset variable and
			// a wrong one need different fixes, so the message has to tell
			// them apart.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not report %s", err, tc.want)
			}

			if !strings.Contains(err.Error(), "MAX_INSTANCES") {
				t.Errorf("error %q does not name the variable", err)
			}
		})
	}

	// And one is accepted, with surrounding space, because a Terraform-rendered
	// value is a string somebody could indent.
	if _, err := Load(env(map[string]string{"MAX_INSTANCES": " 1 "})); err != nil {
		t.Errorf("MAX_INSTANCES=\" 1 \" was refused: %v", err)
	}
}

// An import is a second process on purpose, and needs no ceiling.
//
// It serves no HTTP, completes no authorization flow and runs no sweep, so
// none of the four assumptions the ceiling protects apply to it. Requiring the
// variable would mean inventing a value for a check that is not about it.
func TestMaxInstancesIsNotRequiredForAnImport(t *testing.T) {
	t.Parallel()

	if _, err := LoadImport(env(map[string]string{"MAX_INSTANCES": ""})); err != nil {
		t.Errorf("an import refused to run without MAX_INSTANCES: %v", err)
	}
}

// A store-only process reads where Firestore is and nothing else, and still
// fails closed on a database named without a project.
func TestLoadStoreReadsOnlyFirestore(t *testing.T) {
	t.Parallel()

	cfg, err := LoadStore(func(name string) string {
		switch name {
		case "FIRESTORE_PROJECT":
			return "titelheld-test"
		case "FIRESTORE_DATABASE":
			return "titelheld"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("LoadStore with only Firestore set: %v", err)
	}

	if !cfg.PersistentStore() || cfg.FirestoreDatabase != "titelheld" {
		t.Errorf("cfg = %+v, want the Firestore settings", cfg)
	}

	if _, err := LoadStore(func(name string) string {
		if name == "FIRESTORE_DATABASE" {
			return "titelheld"
		}

		return ""
	}); err == nil {
		t.Error("a database without a project loaded; it should refuse the in-memory fallback")
	}
}

// LOG_PROMPT follows the dry run unless it is set, and it is its own switch.
//
// Its own parser rather than DRY_RUN's, which inverts its input: reusing that
// one would make LOG_PROMPT=1 mean "do not log". This asserts the two read the
// same way round.
func TestLogPromptFollowsDryRunAndOverrides(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "unset, dry run", env: nil, want: true},
		{name: "unset, writes enabled", env: map[string]string{"DRY_RUN": "0"}, want: false},
		{name: "on, writes enabled", env: map[string]string{"DRY_RUN": "0", "LOG_PROMPT": "1"}, want: true},
		{name: "off, dry run", env: map[string]string{"LOG_PROMPT": "0"}, want: false},
		{name: "true", env: map[string]string{"LOG_PROMPT": "true"}, want: true},
		{name: "yes", env: map[string]string{"LOG_PROMPT": "yes"}, want: true},
		{name: "on", env: map[string]string{"LOG_PROMPT": "on"}, want: true},
		{name: "false", env: map[string]string{"LOG_PROMPT": "false"}, want: false},
		{name: "no", env: map[string]string{"LOG_PROMPT": "no"}, want: false},
		{name: "off", env: map[string]string{"LOG_PROMPT": "off"}, want: false},
		{name: "surrounding space", env: map[string]string{"LOG_PROMPT": " 1 "}, want: true},
		{name: "upper case", env: map[string]string{"LOG_PROMPT": "TRUE"}, want: true},

		// The two switches read the same way round. DRY_RUN=1 means writes are
		// off; LOG_PROMPT=1 means logging is on. A shared parser would have
		// made this pair contradict itself.
		{name: "both set, both meaning on", env: map[string]string{"DRY_RUN": "1", "LOG_PROMPT": "1"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(env(tc.env))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if cfg.LogPrompt != tc.want {
				t.Errorf("LogPrompt = %v, want %v", cfg.LogPrompt, tc.want)
			}
		})
	}
}

// An unparseable LOG_PROMPT is a startup error naming the variable.
//
// Not a silent default: the setting decides whether the observation window
// records anything, and a typo that quietly turned it off would be discovered
// by having no evidence when it was wanted.
func TestABadLogPromptIsAStartupError(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"maybe", "2", "onn", "-1"} {
		_, err := Load(env(map[string]string{"LOG_PROMPT": raw}))
		if err == nil {
			t.Errorf("LOG_PROMPT=%q was accepted", raw)

			continue
		}

		if !strings.Contains(err.Error(), "LOG_PROMPT") {
			t.Errorf("error for %q does not name the variable: %v", raw, err)
		}

		if !strings.Contains(err.Error(), raw) {
			t.Errorf("error for %q does not report what it saw: %v", raw, err)
		}
	}
}

// With LLM_PROVIDER unset the resolution is Vertex, exactly as before the
// third provider existed: no key is required, none is read, and the base URL
// is not consulted. The control is the same environment with openrouter
// named, which flips every one of those.
func TestLLMDormancyWithTheProviderUnset(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{"LLM_PROVIDER": "", "LLM_API_KEY": "", "LLM_BASE_URL": "http://plain.example"}))
	if err != nil {
		t.Fatalf("Load with the provider unset: %v", err)
	}

	if cfg.LLM.Provider != ProviderVertex || cfg.LLM.Provider.Keyed() {
		t.Errorf("provider = %q, want the keyless %q", cfg.LLM.Provider, ProviderVertex)
	}

	if cfg.LLM.APIKey != "" {
		t.Errorf("a key was read for the keyless provider: %q", cfg.LLM.APIKey)
	}

	// The control: the same environment with openrouter selected is refused
	// twice over — no key, and a base URL that is not https.
	_, err = Load(env(map[string]string{"LLM_PROVIDER": "openrouter", "LLM_API_KEY": "", "LLM_BASE_URL": "http://plain.example"}))
	if err == nil {
		t.Fatal("openrouter without a key and with a plain-http base URL loaded")
	}

	for _, want := range []string{EnvLLMAPIKey, EnvLLMProvider + "=openrouter", "unset", EnvLLMBaseURL, "https"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not carry %q: %v", want, err)
		}
	}
}

func TestLLMOpenRouterWithAKey(t *testing.T) {
	t.Parallel()

	cfg, err := Load(env(map[string]string{
		"LLM_PROVIDER": "openrouter", "LLM_API_KEY": "k",
		"LLM_MODEL": "google/gemini-3.7-flash", "LLM_BASE_URL": "https://gateway.example:8443/v1/",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.LLM.Provider != ProviderOpenRouter || !cfg.LLM.Provider.Keyed() || cfg.LLM.APIKey != "k" ||
		cfg.LLM.Model != "google/gemini-3.7-flash" || cfg.LLM.BaseURL != "https://gateway.example:8443/v1" {
		t.Errorf("llm = %+v", cfg.LLM)
	}

	// Unset means the provider's own default, which lives in the naming
	// package rather than here.
	cfg, err = Load(env(map[string]string{"LLM_PROVIDER": "openrouter", "LLM_API_KEY": "k"}))
	if err != nil || cfg.LLM.BaseURL != "" {
		t.Errorf("base URL = %q, %v; want empty for the provider default", cfg.LLM.BaseURL, err)
	}
}

// The base URL names the host the key is sent to, so anything but an https
// origin is refused at startup — and only when openrouter is selected.
func TestLLMOpenRouterBaseURLMustBeHTTPS(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"http://openrouter.ai/api/v1", "openrouter.ai/api/v1", "https://", "https://a b/v1", "https://x.example/v1?key=1"} {
		if _, err := Load(env(map[string]string{"LLM_PROVIDER": "openrouter", "LLM_API_KEY": "k", "LLM_BASE_URL": bad})); err == nil {
			t.Errorf("base URL %q was accepted", bad)
		}
	}

	if _, err := Load(env(map[string]string{"LLM_PROVIDER": "anthropic", "LLM_API_KEY": "k", "LLM_BASE_URL": "http://ignored.example"})); err != nil {
		t.Errorf("a base URL was validated for a provider that never reads it: %v", err)
	}
}
