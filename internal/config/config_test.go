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

			var missing *ErrMissing
			if !errors.As(err, &missing) || missing.Name != name {
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
