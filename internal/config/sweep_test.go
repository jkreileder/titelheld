package config

import (
	"strings"
	"testing"
)

const (
	goodSweepPath     = "/sweep/AbCdEf0123456789AbCdEf0123456789"
	goodSweepAudience = "https://namer.example.invalid"
	goodSweepAccount  = "titelheld-scheduler@example.invalid"
)

func sweepEnv(overrides map[string]string) func(string) string {
	values := map[string]string{
		EnvSweepPath:           goodSweepPath,
		EnvSweepAudience:       goodSweepAudience,
		EnvSweepServiceAccount: goodSweepAccount,
	}

	for key, value := range overrides {
		values[key] = value
	}

	return env(values)
}

// All three together is the only configuration that mounts a sweep.
func TestSweepLoadsWhenFullyConfigured(t *testing.T) {
	t.Parallel()

	cfg, err := Load(sweepEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.Sweep.Enabled() {
		t.Fatalf("the sweep is not enabled: %+v", cfg.Sweep)
	}

	if cfg.Sweep.Path != goodSweepPath ||
		cfg.Sweep.Audience != goodSweepAudience ||
		cfg.Sweep.ServiceAccount != goodSweepAccount {
		t.Errorf("Sweep is %+v, want the three values it was given", cfg.Sweep)
	}
}

// None of the three is a local run, not a misconfiguration.
func TestNoSweepSettingsIsNotAnError(t *testing.T) {
	t.Parallel()

	cfg, err := Load(sweepEnv(map[string]string{
		EnvSweepPath:           "",
		EnvSweepAudience:       "",
		EnvSweepServiceAccount: "",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Sweep.Enabled() {
		t.Errorf("the sweep is enabled with nothing configured: %+v", cfg.Sweep)
	}
}

// A partial configuration is an error, and names both halves.
//
// There is no safe reading of it. Ignoring the settings that are present would
// leave the queue undrained with no error anywhere, and filling in the missing
// one would mean inventing the identity this endpoint trusts.
//
// The audience is the exception, and has its own test below.
func TestAPartialSweepConfigurationIsAnError(t *testing.T) {
	t.Parallel()

	for _, missing := range []string{EnvSweepPath, EnvSweepServiceAccount} {
		t.Run("missing "+missing, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(sweepEnv(map[string]string{missing: ""}))
			if err == nil {
				t.Fatal("a partial sweep configuration was accepted")
			}

			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error %q does not name the missing variable", err)
			}

			if cfg.Sweep.Enabled() {
				t.Errorf("the sweep is enabled despite the error: %+v", cfg.Sweep)
			}
		})
	}
}

// A missing audience alone is the first Terraform apply, not a mistake.
//
// The path is generated and the service account exists from the start, so
// Terraform always sets both; the audience is built from the service's own URL,
// which Cloud Run has not minted yet. Terraform produces exactly this
// combination once, by design. Treating it as fatal would stop the apply that
// creates the service — the deployment could never be stood up from scratch.
func TestAMissingAudienceDisablesTheSweepWithoutFailing(t *testing.T) {
	t.Parallel()

	cfg, err := Load(sweepEnv(map[string]string{EnvSweepAudience: ""}))
	if err != nil {
		t.Fatalf("the first-apply configuration was rejected: %v", err)
	}

	if cfg.Sweep.Enabled() {
		t.Errorf("the sweep is enabled with no audience to check against: %+v", cfg.Sweep)
	}

	// Nothing is carried forward, so no later code can read a half-populated
	// configuration and act on the parts that happen to be there.
	if cfg.Sweep != (SweepConfig{}) {
		t.Errorf("Sweep is %+v, want the zero value", cfg.Sweep)
	}
}

// A malformed setting disables the sweep and says which one.
func TestAMalformedSweepSettingIsRejected(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		overrides map[string]string
		wantName  string
	}{
		{
			name:      "a path outside /sweep/",
			overrides: map[string]string{EnvSweepPath: "/webhook/AbCdEf0123456789"},
			wantName:  EnvSweepPath,
		},
		{
			name:      "a path with no segment",
			overrides: map[string]string{EnvSweepPath: "/sweep/"},
			wantName:  EnvSweepPath,
		},
		{
			name:      "a segment that is too short to guess at",
			overrides: map[string]string{EnvSweepPath: "/sweep/short"},
			wantName:  EnvSweepPath,
		},
		{
			name:      "a segment that would need escaping in a URL",
			overrides: map[string]string{EnvSweepPath: "/sweep/has a space here"},
			wantName:  EnvSweepPath,
		},
		{
			name:      "a plaintext audience",
			overrides: map[string]string{EnvSweepAudience: "http://namer.example.invalid"},
			wantName:  EnvSweepAudience,
		},
		{
			name:      "an audience that is not a URL",
			overrides: map[string]string{EnvSweepAudience: "namer.example.invalid"},
			wantName:  EnvSweepAudience,
		},
		{
			name:      "a service account that is not an email",
			overrides: map[string]string{EnvSweepServiceAccount: "titelheld-scheduler"},
			wantName:  EnvSweepServiceAccount,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(sweepEnv(tc.overrides))
			if err == nil {
				t.Fatal("a malformed sweep setting was accepted")
			}

			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("error %q does not name %s", err, tc.wantName)
			}

			if cfg.Sweep.Enabled() {
				t.Errorf("the sweep is enabled despite the error: %+v", cfg.Sweep)
			}
		})
	}
}

// Enabled is false unless every field is set.
//
// It gates the route, so a field added later that nobody thought to check
// here would mount a sweep with one fewer thing verified.
func TestSweepEnabledRequiresEveryField(t *testing.T) {
	t.Parallel()

	full := SweepConfig{
		Path:           goodSweepPath,
		Audience:       goodSweepAudience,
		ServiceAccount: goodSweepAccount,
	}

	if !full.Enabled() {
		t.Error("a fully populated SweepConfig is not enabled")
	}

	for _, tc := range []struct {
		name  string
		blank func(*SweepConfig)
	}{
		{name: "no path", blank: func(s *SweepConfig) { s.Path = "" }},
		{name: "no audience", blank: func(s *SweepConfig) { s.Audience = "" }},
		{name: "no service account", blank: func(s *SweepConfig) { s.ServiceAccount = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			partial := full
			tc.blank(&partial)

			if partial.Enabled() {
				t.Errorf("%+v reports itself as enabled", partial)
			}
		})
	}

	if (SweepConfig{}).Enabled() {
		t.Error("the zero value reports itself as enabled")
	}
}

// Surrounding whitespace is trimmed rather than making the path unmatchable.
func TestSweepSettingsAreTrimmed(t *testing.T) {
	t.Parallel()

	cfg, err := Load(sweepEnv(map[string]string{
		EnvSweepPath:           "  " + goodSweepPath + "\n",
		EnvSweepAudience:       " " + goodSweepAudience + " ",
		EnvSweepServiceAccount: "\t" + goodSweepAccount + " ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Sweep.Path != goodSweepPath {
		t.Errorf("Path is %q, want %q", cfg.Sweep.Path, goodSweepPath)
	}

	if cfg.Sweep.Audience != goodSweepAudience {
		t.Errorf("Audience is %q, want %q", cfg.Sweep.Audience, goodSweepAudience)
	}
}
