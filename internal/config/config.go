// Package config loads the service's runtime configuration from the process
// environment.
//
// Secrets are read from the environment and nowhere else: on Cloud Run they are
// injected from Secret Manager, and locally they come from the shell. Nothing
// here reads or writes a file, so a secret has no route into the working tree.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// pathSecretPattern bounds WEBHOOK_PATH_SECRET to characters that are safe in a
// URL path segment.
//
// This is not cosmetic. http.ServeMux parses its patterns: a segment containing
// a space registers as a malformed pattern and panics, and a segment of the
// form {x} registers as a *wildcard*, which would match any path and remove the
// unguessable-path defence entirely. Both would surface as a crash loop or a
// silent hole after the service had already logged a healthy start.
var pathSecretPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

// Defaults applied when the corresponding variable is unset.
const (
	DefaultPort         = 8080
	DefaultProcessDelay = 10 * time.Minute
)

// Config is the fully resolved configuration.
//
// WritesEnabled is expressed positively so that the zero value of this struct
// is the safe one: a Config nobody filled in cannot write to Strava.
type Config struct {
	// Port is the HTTP port to listen on. Cloud Run sets PORT.
	Port int

	// BaseURL is this service's public base URL, used to build the OAuth
	// redirect. No trailing slash.
	BaseURL string

	// WebhookPath is the full, unguessable path the Strava subscription posts
	// to, including the secret segment.
	WebhookPath string

	// AuthPath is the unguessable path that starts the one-time authorization.
	// The callback stays at the fixed [AuthCallbackPath], because that URL is
	// registered with Strava.
	AuthPath string

	// AthleteID, when set, is the only athlete whose events are accepted.
	// Zero means "accept whichever athlete completed the OAuth flow".
	AthleteID int64

	// ProcessDelay is how long to wait after an event before naming, so the
	// other automations in the chain have finished writing.
	ProcessDelay time.Duration

	// WritesEnabled permits Strava writes. False — the zero value — is dry run.
	WritesEnabled bool

	// StravaClientID and StravaClientSecret identify the Strava API
	// application.
	StravaClientID     string
	StravaClientSecret string

	// StravaVerifyToken is the shared secret echoed during the subscription
	// validation handshake.
	StravaVerifyToken string

	// FirestoreProject and FirestoreDatabase select the Firestore database
	// that holds the OAuth token pair. When the project is empty the service
	// runs on the in-memory store, which forgets everything on restart and is
	// only appropriate for local runs.
	FirestoreProject  string
	FirestoreDatabase string
}

// PersistentStore reports whether state will survive a restart.
func (c Config) PersistentStore() bool {
	return c.FirestoreProject != ""
}

// DryRun reports whether writes are suppressed.
func (c Config) DryRun() bool {
	return !c.WritesEnabled
}

// RedirectURL is the OAuth callback this service exposes.
func (c Config) RedirectURL() string {
	return c.BaseURL + AuthCallbackPath
}

// Environment variable names, named once so the loader, its errors and the
// documentation cannot drift apart. These are the names of variables, never
// values, which is why the credential-detection lint is silenced below.
//
//nolint:gosec // G101: these are variable names, not hardcoded credentials
const (
	EnvStravaClientID     = "STRAVA_CLIENT_ID"
	EnvStravaClientSecret = "STRAVA_CLIENT_SECRET"
	EnvStravaVerifyToken  = "STRAVA_VERIFY_TOKEN"
	EnvStravaAthleteID    = "STRAVA_ATHLETE_ID"
	EnvWebhookPathSecret  = "WEBHOOK_PATH_SECRET"
	EnvBaseURL            = "BASE_URL"
	EnvProcessDelay       = "PROCESS_DELAY"
	EnvDryRun             = "DRY_RUN"
	EnvPort               = "PORT"
	EnvFirestoreProject   = "FIRESTORE_PROJECT"
	EnvFirestoreDatabase  = "FIRESTORE_DATABASE"
)

// Fixed paths, so the OAuth redirect and the router cannot drift apart.
const (
	AuthCallbackPath = "/auth/callback"
	HealthPath       = "/healthz"

	// authCallbackSegment is the one path secret that would collide with the
	// callback route.
	authCallbackSegment = "callback"
)

// ErrMissing reports a required variable that was not set.
type ErrMissing struct {
	Name string
}

func (e *ErrMissing) Error() string {
	return "config: " + e.Name + " is required but not set"
}

// Load resolves the configuration.
//
// getenv is injected rather than calling os.Getenv directly so tests need no
// process-wide environment mutation. Pass os.Getenv in production.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Port:               DefaultPort,
		ProcessDelay:       DefaultProcessDelay,
		StravaClientID:     strings.TrimSpace(getenv(EnvStravaClientID)),
		StravaClientSecret: getenv(EnvStravaClientSecret),
		StravaVerifyToken:  getenv(EnvStravaVerifyToken),
		BaseURL:            strings.TrimRight(strings.TrimSpace(getenv(EnvBaseURL)), "/"),
		FirestoreProject:   strings.TrimSpace(getenv(EnvFirestoreProject)),
		FirestoreDatabase:  strings.TrimSpace(getenv(EnvFirestoreDatabase)),
	}

	var errs []error

	for name, value := range map[string]string{
		EnvStravaClientID:     cfg.StravaClientID,
		EnvStravaClientSecret: cfg.StravaClientSecret,
		EnvStravaVerifyToken:  cfg.StravaVerifyToken,
		EnvBaseURL:            cfg.BaseURL,
	} {
		if value == "" {
			errs = append(errs, &ErrMissing{Name: name})
		}
	}

	pathSecret := strings.Trim(strings.TrimSpace(getenv(EnvWebhookPathSecret)), "/")

	switch {
	case pathSecret == "":
		errs = append(errs, &ErrMissing{Name: EnvWebhookPathSecret})
	case !pathSecretPattern.MatchString(pathSecret):
		errs = append(errs, errors.New(
			"config: "+EnvWebhookPathSecret+" must be 8 to 128 characters from A-Z a-z 0-9 . _ ~ -"))
	case pathSecret == authCallbackSegment:
		errs = append(errs, errors.New(
			"config: "+EnvWebhookPathSecret+" must not be \"callback\": it would collide with the OAuth callback route"))
	default:
		cfg.WebhookPath = "/webhook/" + pathSecret
		cfg.AuthPath = "/auth/" + pathSecret
	}

	if raw := strings.TrimSpace(getenv(EnvPort)); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 || port > 65535 {
			errs = append(errs, fmt.Errorf("config: "+EnvPort+" must be a valid port number, got %q", raw))
		} else {
			cfg.Port = port
		}
	}

	if raw := strings.TrimSpace(getenv(EnvStravaAthleteID)); raw != "" {
		athleteID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || athleteID <= 0 {
			errs = append(errs,
				fmt.Errorf("config: "+EnvStravaAthleteID+" must be a positive integer, got %q", raw))
		} else {
			cfg.AthleteID = athleteID
		}
	}

	if raw := strings.TrimSpace(getenv(EnvProcessDelay)); raw != "" {
		delay, err := time.ParseDuration(raw)
		if err != nil || delay < 0 {
			errs = append(errs,
				fmt.Errorf("config: "+EnvProcessDelay+" must be a non-negative duration, got %q", raw))
		} else {
			cfg.ProcessDelay = delay
		}
	}

	writesEnabled, err := parseWritesEnabled(getenv(EnvDryRun))
	if err != nil {
		errs = append(errs, err)
	}

	cfg.WritesEnabled = writesEnabled

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	return cfg, nil
}

// parseWritesEnabled interprets DRY_RUN.
//
// Dry run is the default and stays on unless the variable holds an explicit,
// unambiguous falsy value. An unset variable, an empty one, or a value that
// cannot be read as a boolean all leave writes disabled — a typo must never be
// the thing that lets this service loose on real activities. A typo is still
// reported, so it does not pass unnoticed.
func parseWritesEnabled(raw string) (bool, error) {
	value := strings.ToLower(strings.TrimSpace(raw))

	switch value {
	case "":
		return false, nil
	case "1", "t", "true", "y", "yes", "on":
		return false, nil
	case "0", "f", "false", "n", "no", "off":
		return true, nil
	default:
		return false, fmt.Errorf(
			"config: "+EnvDryRun+" must be a boolean, got %q (staying in dry run)", raw)
	}
}
