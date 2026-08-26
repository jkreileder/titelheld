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
	"slices"
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
// unguessable-path defense entirely. Both would surface as a crash loop or a
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

	// Sweep is the scheduled sweep's route and the identity allowed to call
	// it. All three are set together by Terraform or not at all; see
	// [SweepConfig].
	Sweep SweepConfig

	// AthleteID, when set, is the only athlete whose events are accepted.
	// Zero means "accept whichever athlete completed the OAuth flow".
	AthleteID int64

	// ProcessDelay is how long to wait after an event before naming, so the
	// other automations in the chain have finished writing.
	ProcessDelay time.Duration

	// WritesEnabled permits Strava writes. False — the zero value — is dry run.
	WritesEnabled bool

	// LogPrompt logs the complete prompt sent for each naming.
	//
	// On by default while writes are off, because that is the observation
	// window and its whole purpose is judging what the model was given rather
	// than inferring it from counters. LOG_PROMPT forces it either way.
	//
	// Verbosity is what this gates. A prompt is the athlete's own material:
	// their ride, their gear name, titles they have used, and place names the
	// geo layer resolved — that layer produces names and has nowhere to hold a
	// coordinate.
	//
	// One value is not this service's to vouch for. A NOTES fact has an
	// allow-listed label and a free-text value, taken from a description
	// another tool wrote, so a tool that puts a coordinate in one puts it in
	// the prompt and therefore in the log.
	LogPrompt bool

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

	// LLM configures the naming layer: which provider, which model, and the
	// lists that shape a title. See llm.go.
	LLM LLM

	// NominatimUserAgent identifies this service to Nominatim, whose usage
	// policy requires a real, contactable identity. It defaults to the service
	// name plus BaseURL, which is contactable enough for a single-athlete
	// deployment; override it to point at a mailbox.
	NominatimUserAgent string
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

// SweepConfig is what the sweep route needs to exist and to say no.
//
// Every field is safe at its zero value, and the zero value is "no sweep
// route". That matters more here than anywhere else in this file: the sweep
// is the one endpoint that makes this service write to Strava on its own
// initiative, and the alternative reading of an unset audience — accept any
// audience — would turn a half-configured deployment into an open trigger.
//
// The audience specifically is empty on the very first Terraform apply,
// because the service URL it is built from does not exist until Cloud Run has
// minted it, while the generated path and the service account are known from
// the start. An empty audience therefore means "no sweep route yet" rather
// than a misconfiguration; refusing to mount the route until the second apply
// is the correct behavior for that window, not a limitation of it. Any other
// gap is a mistake and is reported as one.
type SweepConfig struct {
	// Path is the full, unguessable path Cloud Scheduler posts to, including
	// the generated segment. Terraform mints it; it is never configured by
	// hand and never appears in the repository.
	Path string

	// Audience is the exact audience the Scheduler's OIDC token must carry.
	// Terraform feeds one value to both sides so they cannot drift.
	Audience string

	// ServiceAccount is the email of the identity allowed to trigger a sweep.
	// The scheduler account, which holds invoke permission and nothing else —
	// notably not the runtime account, which can read the athlete's data.
	ServiceAccount string
}

// Enabled reports whether the sweep route may be mounted.
//
// Anything less than all three is a misconfiguration, and the route not
// existing is how it reports: a 404 is unambiguous in a log, where a mounted
// route that rejects everything looks exactly like an attack.
func (s SweepConfig) Enabled() bool {
	return s.Path != "" && s.Audience != "" && s.ServiceAccount != ""
}

// Environment variable names, named once so the loader, its errors and the
// documentation cannot drift apart. These are the names of variables, never
// values, which is why the credential-detection lint is silenced below.
//
//nolint:gosec // G101: these are variable names, not hardcoded credentials
const (
	EnvStravaClientID      = "STRAVA_CLIENT_ID"
	EnvStravaClientSecret  = "STRAVA_CLIENT_SECRET"
	EnvStravaVerifyToken   = "STRAVA_VERIFY_TOKEN"
	EnvStravaAthleteID     = "STRAVA_ATHLETE_ID"
	EnvWebhookPathSecret   = "WEBHOOK_PATH_SECRET"
	EnvBaseURL             = "BASE_URL"
	EnvProcessDelay        = "PROCESS_DELAY"
	EnvDryRun              = "DRY_RUN"
	EnvPort                = "PORT"
	EnvFirestoreProject    = "FIRESTORE_PROJECT"
	EnvFirestoreDatabase   = "FIRESTORE_DATABASE"
	EnvNominatimUserAgent  = "NOMINATIM_USER_AGENT"
	EnvSweepPath           = "SWEEP_PATH"
	EnvSweepAudience       = "SWEEP_AUDIENCE"
	EnvSweepServiceAccount = "SWEEP_SERVICE_ACCOUNT"
	EnvMaxInstances        = "MAX_INSTANCES"
	EnvLogPrompt           = "LOG_PROMPT"
)

// RequiredMaxInstances is the only value this service will start with.
//
// Not a tuning knob: four pieces of state live in the process and are correct
// only when one instance is serving.
//
//   - [server.Server.states] holds the OAuth state parameters this process
//     issued. A second instance would reject a callback it did not issue,
//     turning the one-time authorization flow into a coin flip.
//   - [server.Server.bind] serializes the first-bind decision. Two processes
//     can both read "nothing is bound yet" and bind different athletes.
//   - [strava.StoredTokenSource.mu] serializes token refresh. Strava rotates
//     the refresh token on every refresh, so two processes refreshing at once
//     invalidate each other's token and the athlete has to reauthorize.
//   - [sweep.Handler.running] answers an overlapping fire with "already
//     running" instead of sweeping twice. It is a mutex in this process, so a
//     second process sweeps the same queue in parallel — and two sweeps can
//     both read the named log for an activity before either writes it.
//
// Terraform tells Cloud Run `max_instance_count = 1` and passes the same
// number in here, so the ceiling and what the binary believes cannot drift.
//
// This is a deployment check and not a mutex. The ceiling is per revision, so
// two revisions serving at once — a rolling deploy, a traffic split — are two
// instances that each read 1 and each start happily, and each of the four
// states above then fails the way its bullet says. What keeps that tolerable
// is the deployment pattern rather than this code: single-revision releases,
// no traffic split, a paused scheduler. None of it is enforced here, and the
// fixes that would be — a compare-and-set on the token document, a lease for
// the sweep — are not built.
const RequiredMaxInstances = "1"

// Fixed paths, so the OAuth redirect and the router cannot drift apart.
const (
	AuthCallbackPath = "/auth/callback"

	// HealthPath is deliberately not "/healthz". Cloud Run's frontend answers
	// that exact path itself: a request for it never reaches the container,
	// returns Google's HTML error page rather than this service's plain-text
	// one, and produces no request log line — while "/healthz/", "/health"
	// and every other path arrive normally. The health route was therefore
	// unreachable in production from the first deploy, and nothing noticed
	// because nothing depends on it: Cloud Run probes the port, not a path.
	HealthPath = "/health"

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

// checkMaxInstances refuses to start on anything but the one supported value.
//
// It reports what it saw. "MAX_INSTANCES must be 1" sends an operator to look
// at a variable that may not be set at all, and the two causes need different
// fixes: an unset variable means the Terraform has not been applied since the
// contract landed, while a wrong one means somebody scaled the service on
// purpose and needs to read [RequiredMaxInstances] before doing that.
func checkMaxInstances(raw string) error {
	value := strings.TrimSpace(raw)

	switch {
	case value == "":
		return errors.New("config: " + EnvMaxInstances + " is not set; Cloud Run must be " +
			"configured with max_instance_count = " + RequiredMaxInstances +
			" and pass it in, because this service keeps single-instance state - " +
			"apply the Terraform before deploying this revision")
	case value != RequiredMaxInstances:
		return errors.New("config: " + EnvMaxInstances + " is " + strconv.Quote(value) +
			", and this service only runs with " + strconv.Quote(RequiredMaxInstances) +
			": OAuth state, the first-bind lock, token refresh and the sweep lock " +
			"are all in-process and a second instance breaks each of them")
	default:
		return nil
	}
}

// Load resolves the configuration.
//
// getenv is injected rather than calling os.Getenv directly so tests need no
// process-wide environment mutation. Pass os.Getenv in production.
func Load(getenv func(string) string) (Config, error) {
	return load(getenv, true)
}

// LoadImport reads only what a one-shot import needs.
//
// An import serves no HTTP and completes no authorization flow, so the
// webhook's verify token and unguessable path, and the public base URL the
// OAuth redirect is built from, are nothing to it — Strava's token endpoint
// takes no redirect_uri on a refresh. Requiring them would make an operator
// invent values for a job that never reads them, and the invented values
// would sit in a shell history looking meaningful.
//
// Everything else is identical, including the Firestore settings and the
// machine-title patterns the import needs to know which titles to skip.
func LoadImport(getenv func(string) string) (Config, error) {
	return load(getenv, false)
}

// LoadStore reads only what a process that touches nothing but the store
// needs: where Firestore is.
//
// For a one-shot that writes a document under the operator's own credentials
// and calls neither Strava nor anything else — seeding the configuration
// document. Strava's client ID and secret would be invented values for a job
// that never makes a request with them, and every other setting is about
// serving.
//
// The one check shared with the other loaders is the fail-closed one: a
// database named without a project is refused rather than silently answered
// by the in-memory store.
func LoadStore(getenv func(string) string) (Config, error) {
	cfg := Config{
		FirestoreProject:  strings.TrimSpace(getenv(EnvFirestoreProject)),
		FirestoreDatabase: strings.TrimSpace(getenv(EnvFirestoreDatabase)),
	}

	if cfg.FirestoreDatabase != "" && cfg.FirestoreProject == "" {
		return Config{}, errors.New(
			"config: " + EnvFirestoreDatabase + " is set but " + EnvFirestoreProject +
				" is not; refusing to fall back to the in-memory store")
	}

	return cfg, nil
}

// load reads the environment. serving requires the settings only the running
// service uses.
func load(getenv func(string) string, serving bool) (Config, error) {
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

	required := map[string]string{
		EnvStravaClientID:     cfg.StravaClientID,
		EnvStravaClientSecret: cfg.StravaClientSecret,
	}

	if serving {
		required[EnvStravaVerifyToken] = cfg.StravaVerifyToken
		required[EnvBaseURL] = cfg.BaseURL
	}

	for name, value := range required {
		if value == "" {
			errs = append(errs, &ErrMissing{Name: name})
		}
	}

	// Serving only. An import is a deliberate second process — it runs by
	// hand, against the same Firestore, while the service is idle — and it
	// touches none of the four pieces of state the ceiling protects: it serves
	// no HTTP, completes no authorization flow and runs no sweep. Requiring
	// the variable there would mean inventing a value to satisfy a check that
	// is not about it.
	//
	// A passing check means the ceiling is configured, not that this process
	// is alone; see [RequiredMaxInstances] for what a revision overlap still
	// permits.
	if serving {
		if err := checkMaxInstances(getenv(EnvMaxInstances)); err != nil {
			errs = append(errs, err)
		}
	}

	pathSecret := strings.Trim(strings.TrimSpace(getenv(EnvWebhookPathSecret)), "/")

	switch {
	case pathSecret == "" && !serving:
		// Nothing to serve, so nothing to guard with an unguessable path.
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

	cfg.Sweep = loadSweep(getenv, &errs)

	cfg.LLM = loadLLM(getenv, cfg.FirestoreProject, &errs)

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

	// Failing closed: a database named without a project would otherwise start
	// cleanly on the in-memory store and drop the rotated refresh token at the
	// first restart — the one failure this package exists to prevent.
	if cfg.FirestoreDatabase != "" && cfg.FirestoreProject == "" {
		errs = append(errs, errors.New(
			"config: "+EnvFirestoreDatabase+" is set but "+EnvFirestoreProject+
				" is not; refusing to fall back to the in-memory store"))
	}

	cfg.NominatimUserAgent = strings.TrimSpace(getenv(EnvNominatimUserAgent))
	if cfg.NominatimUserAgent == "" && cfg.BaseURL != "" {
		// Nominatim blocks anonymous clients, so there is always an identity;
		// the deployment URL is the most useful default available here.
		cfg.NominatimUserAgent = "titelheld/1.0 (+" + cfg.BaseURL + ")"
	}

	writesEnabled, err := parseWritesEnabled(getenv(EnvDryRun))
	if err != nil {
		errs = append(errs, err)
	}

	cfg.WritesEnabled = writesEnabled

	// Defaults to the dry-run state — after WritesEnabled is known, because it
	// is derived from it. The observation window then logs prompts without
	// anyone remembering to ask for it, and a service with writes on does not
	// unless somebody says so.
	cfg.LogPrompt = !cfg.WritesEnabled

	if raw := strings.TrimSpace(getenv(EnvLogPrompt)); raw != "" {
		logPrompt, err := parseLogPrompt(raw)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.LogPrompt = logPrompt
		}
	}

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

// parseLogPrompt reads LOG_PROMPT.
//
// Spelled out rather than reusing parseWritesEnabled, which inverts its input:
// DRY_RUN=1 means writes are off, and reusing it here would make LOG_PROMPT=1
// mean "do not log". Two settings that read the same way in the environment
// must not mean opposite things.
func parseLogPrompt(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf(
			"config: "+EnvLogPrompt+" must be a boolean, got %q", raw)
	}
}

// sweepSegmentPattern is what a generated sweep segment must look like.
//
// Deliberately the same alphabet as the webhook secret: both are URL path
// segments minted by Terraform, and a value that needs escaping to appear in a
// URL would mean the path the service serves and the path the scheduler posts
// to are different strings.
var sweepSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

// loadSweep reads the three sweep settings, which stand or fall together.
//
// Unset means no sweep route, and is not an error: that is a local run, or a
// first Terraform apply before Cloud Run has minted the URL the audience is
// built from. A *partial* configuration is an error, because there is no
// reading of it that is safe. Ignoring it would leave the queue silently
// undrained, and inferring the missing piece would mean inventing the identity
// this endpoint trusts.
func loadSweep(getenv func(string) string, errs *[]error) SweepConfig {
	sweep := SweepConfig{
		Path:           strings.TrimSpace(getenv(EnvSweepPath)),
		Audience:       strings.TrimSpace(getenv(EnvSweepAudience)),
		ServiceAccount: strings.TrimSpace(getenv(EnvSweepServiceAccount)),
	}

	set := make([]string, 0, 3)
	unset := make([]string, 0, 3)

	for name, value := range map[string]string{
		EnvSweepPath:           sweep.Path,
		EnvSweepAudience:       sweep.Audience,
		EnvSweepServiceAccount: sweep.ServiceAccount,
	} {
		if value == "" {
			unset = append(unset, name)
		} else {
			set = append(set, name)
		}
	}

	if len(set) == 0 {
		return SweepConfig{}
	}

	// The first Terraform apply, and only that. The path is generated and the
	// service account exists from the start, so Terraform always sets both;
	// the audience is built from the service's own URL, which Cloud Run has
	// not minted yet. That one combination is produced by design, and treating
	// it as fatal would stop the very apply that creates the service.
	//
	// The other two fields are required here rather than assumed, or the test
	// would forgive any configuration that merely happens to lack an audience
	// — SWEEP_PATH on its own, say, which is a mistake and has to be reported
	// as one. Disabling the route is the safe reading; the unsafe one,
	// accepting any audience, is what never happens.
	if sweep.Audience == "" && sweep.Path != "" && sweep.ServiceAccount != "" {
		return SweepConfig{}
	}

	if len(unset) > 0 {
		slices.Sort(unset)
		*errs = append(*errs, fmt.Errorf(
			"config: the sweep is partly configured: %s set, %s missing"+
				" (set all three or none)",
			strings.Join(sortedCopy(set), ", "), strings.Join(unset, ", ")))

		return SweepConfig{}
	}

	segment, ok := strings.CutPrefix(sweep.Path, "/sweep/")
	if !ok || !sweepSegmentPattern.MatchString(segment) {
		*errs = append(*errs, errors.New(
			"config: "+EnvSweepPath+" must be /sweep/ followed by 8 to 128"+
				" characters from A-Z a-z 0-9 . _ ~ -"))

		return SweepConfig{}
	}

	// An audience that is not the service's own URL means the token was minted
	// for something else, and validating against it would be validating
	// against whatever an operator happened to paste in.
	if !strings.HasPrefix(sweep.Audience, "https://") {
		*errs = append(*errs, fmt.Errorf(
			"config: "+EnvSweepAudience+" must be an https URL, got %q", sweep.Audience))

		return SweepConfig{}
	}

	if !strings.Contains(sweep.ServiceAccount, "@") {
		*errs = append(*errs, fmt.Errorf(
			"config: "+EnvSweepServiceAccount+" must be a service account email, got %q",
			sweep.ServiceAccount))

		return SweepConfig{}
	}

	return sweep
}

// sortedCopy sorts without disturbing the caller's slice, so an error message
// reads the same on every run. Map iteration order is randomized, and an error
// that reorders itself between two runs of the same broken deployment is one
// nobody can diff.
func sortedCopy(values []string) []string {
	return slices.Sorted(slices.Values(values))
}
