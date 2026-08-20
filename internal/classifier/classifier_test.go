package classifier

import (
	"math"
	"testing"
)

// Every coordinate in this file is synthetic. Home sits on Null Island and work
// 0.05° north of it (~5.55 km), which is the right order of magnitude for the
// commute cases without disclosing anything real.
var (
	testHome = Geofence{Center: Point{Lat: 0, Lon: 0}, RadiusMeters: 300}
	testWork = Geofence{Center: Point{Lat: 0.05, Lon: 0}, RadiusMeters: 300}

	atHome    = &Point{Lat: 0.0005, Lon: 0.0005}
	atWork    = &Point{Lat: 0.0503, Lon: 0.0002}
	elsewhere = &Point{Lat: 1.2345, Lon: 6.7890}
)

// geofencedConfig is the shipped configuration plus the synthetic geofences, so
// the commute safety net is exercisable.
func geofencedConfig() Config {
	cfg := DefaultConfig()
	cfg.Home = testHome
	cfg.Work = testWork

	return cfg
}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		activity      Activity
		cfg           Config
		wantTier      Tier
		wantAction    Action
		wantDirection Direction
		wantReason    string
	}{
		// --- The six cases named in the acceptance criteria. ---
		{
			name: "tier 5: 67.6 km gravel ride still at its Strava default",
			activity: Activity{
				Name:              "Afternoon Ride",
				SportType:         "GravelRide",
				DistanceMeters:    67638.5,
				MovingTimeSeconds: 10876,
				Start:             elsewhere,
				End:               elsewhere,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},
		{
			name: "tier 3: 5.4 km commute already titled by ActivityFix",
			activity: Activity{
				Name:              "Zur Arbeit",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    5406,
				MovingTimeSeconds: 922,
				Start:             atHome,
				End:               atWork,
			},
			cfg:           geofencedConfig(),
			wantTier:      TierCommute,
			wantAction:    ActionSkip,
			wantDirection: DirectionToWork,
			wantReason:    "title is not a Strava default",
		},
		{
			name: "tier 4: 0.96 km commute-tagged errand at its Strava default",
			activity: Activity{
				Name:              "Lunch Ride",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    960.7,
				MovingTimeSeconds: 208,
				Start:             elsewhere,
				End:               elsewhere,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierErrand,
			wantAction: ActionErrandTemplate,
			wantReason: "commute-tagged errand",
		},
		{
			name: "tier 1: Whoop weight training",
			activity: Activity{
				Name:              "Gewichtstraining am Abend",
				Description:       "12,4 Strain amounts to moderate exertion today.",
				SportType:         "WeightTraining",
				DistanceMeters:    142.4,
				MovingTimeSeconds: 334,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "sport type WeightTraining is never named",
		},
		{
			name: "tier 2: virtual trainer ride, shipped zwift_mode",
			activity: Activity{
				Name:              "Evening Ride",
				SportType:         "VirtualRide",
				Trainer:           true,
				DistanceMeters:    32000,
				MovingTimeSeconds: 3600,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierVirtual,
			wantAction: ActionSkip,
			wantReason: "virtual ride, zwift_mode=keep",
		},
		{
			name: "skip gate: gravel ride already titled by a franchise run",
			activity: Activity{
				Name:              "The Pink Panther Checks Inn",
				SportType:         "GravelRide",
				DistanceMeters:    67638.5,
				MovingTimeSeconds: 10876,
				Start:             elsewhere,
				End:               elsewhere,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionSkip,
			wantReason: "title is not a Strava default",
		},

		// --- Tier 1: never named. ---
		{
			name:       "tier 1: walk",
			activity:   Activity{Name: "Afternoon Walk", SportType: "Walk", Commute: true},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "sport type Walk is never named",
		},
		{
			name:       "tier 1: hike",
			activity:   Activity{Name: "Morning Hike", SportType: "Hike"},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "sport type Hike is never named",
		},
		{
			name:       "tier 1: workout",
			activity:   Activity{Name: "Morning Workout", SportType: "Workout"},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "sport type Workout is never named",
		},
		{
			name: "tier 1 beats tier 2: strength session recorded on a trainer",
			activity: Activity{
				Name:        "Evening Physical Therapy",
				Description: "11,8 Strain amounts to moderate exertion today.",
				SportType:   "WeightTraining",
				Trainer:     true,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "sport type WeightTraining is never named",
		},
		{
			name: "tier 1 beats tier 5: Whoop marker on a long ride",
			activity: Activity{
				Name:              "Morning Ride",
				Description:       "17,2 Strain amounts to a strenuous day.",
				SportType:         "Ride",
				DistanceMeters:    80000,
				MovingTimeSeconds: 12000,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "Whoop activity (description mentions Strain)",
		},
		{
			name: "tier 1 wins over the skip gate: reason names the sport type",
			activity: Activity{
				Name:      "Kraftraum, Runde 4",
				SportType: "WeightTraining",
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "sport type WeightTraining is never named",
		},

		// --- Tier 2: virtual and indoor. ---
		{
			name: "tier 2: llm_indoor names the ride",
			activity: Activity{
				Name:              "Evening Ride",
				SportType:         "VirtualRide",
				Trainer:           true,
				DistanceMeters:    32000,
				MovingTimeSeconds: 3600,
			},
			cfg: func() Config {
				cfg := geofencedConfig()
				cfg.ZwiftMode = ZwiftLLMIndoor

				return cfg
			}(),
			wantTier:   TierVirtual,
			wantAction: ActionLLMIndoor,
			wantReason: "virtual ride, zwift_mode=llm_indoor",
		},
		{
			name: "tier 2: trainer flag alone, sport type Ride",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				Trainer:           true,
				DistanceMeters:    28000,
				MovingTimeSeconds: 3300,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierVirtual,
			wantAction: ActionSkip,
			wantReason: "virtual ride, zwift_mode=keep",
		},
		{
			name: "tier 2: Zwift's own title is not a Strava default",
			activity: Activity{
				Name:      "Watopia Figure 8 in Watopia",
				SportType: "VirtualRide",
				Trainer:   true,
			},
			cfg: func() Config {
				cfg := geofencedConfig()
				cfg.ZwiftMode = ZwiftLLMIndoor

				return cfg
			}(),
			wantTier:   TierVirtual,
			wantAction: ActionSkip,
			wantReason: "title is not a Strava default",
		},

		{
			name: "tier 2 is ride-scoped: a treadmill run is not a virtual ride",
			activity: Activity{
				Name:              "Morning Run",
				SportType:         "Run",
				Trainer:           true,
				DistanceMeters:    8000,
				MovingTimeSeconds: 2700,
			},
			cfg: func() Config {
				cfg := geofencedConfig()
				cfg.ZwiftMode = ZwiftLLMIndoor

				return cfg
			}(),
			wantTier:   TierNone,
			wantAction: ActionSkip,
			wantReason: "sport type Run has no tier rule",
		},
		{
			name: "tier 2 is ride-scoped: a trainer-flagged walk stays in tier 1",
			activity: Activity{
				Name:      "Evening Walk",
				SportType: "Walk",
				Trainer:   true,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSkip,
			wantAction: ActionSkip,
			wantReason: "sport type Walk is never named",
		},

		// --- Tier 3: commute safety net. ---
		{
			name: "tier 3: ActivityFix failed, ride ends at work",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				DistanceMeters:    5406,
				MovingTimeSeconds: 922,
				Start:             atHome,
				End:               atWork,
			},
			cfg:           geofencedConfig(),
			wantTier:      TierCommute,
			wantAction:    ActionCommuteTemplate,
			wantDirection: DirectionToWork,
			wantReason:    "commute safety net (to_work)",
		},
		{
			name: "tier 3: ActivityFix failed, ride runs work to home",
			activity: Activity{
				Name:              "Evening Ride",
				SportType:         "Ride",
				DistanceMeters:    5836,
				MovingTimeSeconds: 1062,
				Start:             atWork,
				End:               atHome,
			},
			cfg:           geofencedConfig(),
			wantTier:      TierCommute,
			wantAction:    ActionCommuteTemplate,
			wantDirection: DirectionToHome,
			wantReason:    "commute safety net (to_home)",
		},
		{
			name: "tier 3: already-titled ride home",
			activity: Activity{
				Name:              "Nach Hause",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    5836,
				MovingTimeSeconds: 1062,
			},
			cfg:           geofencedConfig(),
			wantTier:      TierCommute,
			wantAction:    ActionSkip,
			wantDirection: DirectionToHome,
			wantReason:    "title is not a Strava default",
		},
		{
			name: "tier 3 is not reached without geofences: falls through to tier 4",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    5406,
				MovingTimeSeconds: 922,
				Start:             atHome,
				End:               atWork,
			},
			cfg:        DefaultConfig(),
			wantTier:   TierErrand,
			wantAction: ActionErrandTemplate,
			wantReason: "commute-tagged errand",
		},
		{
			// The geofence path only infers a commute, so it is capped by the
			// tier-5 thresholds: a long ride that merely finishes at work is a
			// sport ride.
			name: "tier 5 outranks tier 3: a 45 km ride ending at work is not a commute",
			activity: Activity{
				Name:              "Afternoon Ride",
				SportType:         "GravelRide",
				DistanceMeters:    45000,
				MovingTimeSeconds: 7200,
				Start:             elsewhere,
				End:               atWork,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},
		{
			// ... but an ActivityFix-written title is direct evidence and is
			// taken at face value whatever the ride's size.
			name: "tier 3: a long ride ActivityFix titled is still a commute",
			activity: Activity{
				Name:              "Zur Arbeit",
				SportType:         "Ride",
				DistanceMeters:    45000,
				MovingTimeSeconds: 7200,
				Start:             elsewhere,
				End:               atWork,
			},
			cfg:           geofencedConfig(),
			wantTier:      TierCommute,
			wantAction:    ActionSkip,
			wantDirection: DirectionToWork,
			wantReason:    "title is not a Strava default",
		},
		{
			name: "tier 3: a ride just over the distance threshold ending at work",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				DistanceMeters:    15000,
				MovingTimeSeconds: 1800,
				Start:             atHome,
				End:               atWork,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},
		{
			name: "tier 3: a ride that merely passes near work is not a commute",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				DistanceMeters:    42000,
				MovingTimeSeconds: 6000,
				Start:             atHome,
				End:               atHome,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},

		{
			name: "tier 3: an empty commute title does not match an untitled activity",
			activity: Activity{
				Name:              "",
				SportType:         "Ride",
				DistanceMeters:    42000,
				MovingTimeSeconds: 6000,
			},
			cfg: func() Config {
				cfg := geofencedConfig()
				cfg.ToWorkTitle = ""
				cfg.ToHomeTitle = ""

				return cfg
			}(),
			wantTier:   TierSportRide,
			wantAction: ActionSkip,
			wantReason: "title is not a Strava default",
		},
		{
			name: "tier 3: clearing the commute titles disables the title match",
			activity: Activity{
				Name:              "Zur Arbeit",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    5406,
				MovingTimeSeconds: 922,
			},
			cfg: func() Config {
				cfg := geofencedConfig()
				cfg.ToWorkTitle = ""
				cfg.ToHomeTitle = ""

				return cfg
			}(),
			wantTier:   TierErrand,
			wantAction: ActionSkip,
			wantReason: "title is not a Strava default",
		},

		// --- Tier 4: errands. ---
		{
			name: "tier 4: gravel errand at its Strava default",
			activity: Activity{
				Name:              "Afternoon Gravel Ride",
				SportType:         "GravelRide",
				Commute:           true,
				DistanceMeters:    2323.3,
				MovingTimeSeconds: 448,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierErrand,
			wantAction: ActionErrandTemplate,
			wantReason: "commute-tagged errand",
		},
		{
			name: "tier 4: errand naming switched off",
			activity: Activity{
				Name:              "Lunch Ride",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    960.7,
				MovingTimeSeconds: 208,
			},
			cfg: func() Config {
				cfg := geofencedConfig()
				cfg.LeaveErrandsUnnamed = true

				return cfg
			}(),
			wantTier:   TierErrand,
			wantAction: ActionSkip,
			wantReason: "errand naming disabled",
		},
		{
			name: "tier 4 beats tier 5: a long ride tagged as a commute stays an errand",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    31000,
				MovingTimeSeconds: 5400,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierErrand,
			wantAction: ActionErrandTemplate,
			wantReason: "commute-tagged errand",
		},

		// --- Tier 5: sport rides, including both thresholds. ---
		{
			name: "tier 5: exactly at the distance threshold",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				DistanceMeters:    15000,
				MovingTimeSeconds: 1800,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},
		{
			name: "tier 5: exactly at the moving-time threshold",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				DistanceMeters:    9000,
				MovingTimeSeconds: 2700,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},
		{
			name: "tier 5: custom thresholds from config",
			activity: Activity{
				Name:              "Morning Ride",
				SportType:         "Ride",
				DistanceMeters:    9000,
				MovingTimeSeconds: 1500,
			},
			cfg: func() Config {
				cfg := geofencedConfig()
				cfg.SportMinDistanceMeters = 8000
				cfg.SportMinMovingTimeSeconds = 1200

				return cfg
			}(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},

		// --- No tier: nothing is written. ---
		{
			name: "no tier: short non-commute ride just under both thresholds",
			activity: Activity{
				Name:              "Evening Ride",
				SportType:         "Ride",
				DistanceMeters:    14999.9,
				MovingTimeSeconds: 2699,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierNone,
			wantAction: ActionSkip,
			wantReason: "ride below the sport-ride thresholds",
		},
		{
			name: "no tier: a run has no rule of its own",
			activity: Activity{
				Name:              "Morning Run",
				SportType:         "Run",
				DistanceMeters:    12000,
				MovingTimeSeconds: 3600,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierNone,
			wantAction: ActionSkip,
			wantReason: "sport type Run has no tier rule",
		},
		{
			name: "no tier: a commute-tagged run does not become an errand",
			activity: Activity{
				Name:              "Lunch Run",
				SportType:         "Run",
				Commute:           true,
				DistanceMeters:    1500,
				MovingTimeSeconds: 600,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierNone,
			wantAction: ActionSkip,
			wantReason: "sport type Run has no tier rule",
		},
		{
			name: "no tier: a swim has no rule of its own",
			activity: Activity{
				Name:              "Morning Swim",
				SportType:         "Swim",
				DistanceMeters:    1500,
				MovingTimeSeconds: 2400,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierNone,
			wantAction: ActionSkip,
			wantReason: "sport type Swim has no tier rule",
		},

		// --- Skip gate details. ---
		{
			name: "skip gate: an empty title is not a default",
			activity: Activity{
				Name:              "",
				SportType:         "Ride",
				DistanceMeters:    42000,
				MovingTimeSeconds: 6000,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionSkip,
			wantReason: "title is not a Strava default",
		},
		{
			name: "skip gate: surrounding whitespace does not defeat the default match",
			activity: Activity{
				Name:              "  Morning Ride  ",
				SportType:         "Ride",
				DistanceMeters:    42000,
				MovingTimeSeconds: 6000,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},
		{
			name: "skip gate: an Xert-rewritten title is left alone",
			activity: Activity{
				Name:              "Threshold 3x12 (Xert)",
				SportType:         "Ride",
				DistanceMeters:    42000,
				MovingTimeSeconds: 6000,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionSkip,
			wantReason: "title is not a Strava default",
		},
		{
			name: "skip gate: a localized default title still qualifies",
			activity: Activity{
				Name:              "Radfahrt am Nachmittag",
				SportType:         "Ride",
				DistanceMeters:    42000,
				MovingTimeSeconds: 6000,
			},
			cfg:        geofencedConfig(),
			wantTier:   TierSportRide,
			wantAction: ActionLLM,
			wantReason: "sport ride",
		},

		// --- A zero Config must be as safe as the shipped one. ---
		{
			name: "zero config: thresholds fall back to the shipped defaults",
			activity: Activity{
				Name:              "Evening Ride",
				SportType:         "Ride",
				DistanceMeters:    3000,
				MovingTimeSeconds: 600,
			},
			cfg:        Config{},
			wantTier:   TierNone,
			wantAction: ActionSkip,
			wantReason: "ride below the sport-ride thresholds",
		},
		{
			name: "zero config: commute titles are not filled in, so no tier 3",
			activity: Activity{
				Name:              "Nach Hause",
				SportType:         "Ride",
				Commute:           true,
				DistanceMeters:    5836,
				MovingTimeSeconds: 1062,
			},
			cfg:        Config{},
			wantTier:   TierErrand,
			wantAction: ActionSkip,
			wantReason: "title is not a Strava default",
		},
		{
			name: "zero config: virtual rides are kept",
			activity: Activity{
				Name:      "Morning Ride",
				SportType: "VirtualRide",
			},
			cfg:        Config{},
			wantTier:   TierVirtual,
			wantAction: ActionSkip,
			wantReason: "virtual ride, zwift_mode=keep",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Classify(tt.activity, tt.cfg)

			if got.Tier != tt.wantTier {
				t.Errorf("Tier = %v, want %v", got.Tier, tt.wantTier)
			}
			if got.Action != tt.wantAction {
				t.Errorf("Action = %v, want %v", got.Action, tt.wantAction)
			}
			if got.Direction != tt.wantDirection {
				t.Errorf("Direction = %v, want %v", got.Direction, tt.wantDirection)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// TestClassifyNeverWritesOverAnExistingTitle is the belt-and-braces check on the
// skip gate: whatever the tier rules make of an activity, a title that is not a
// Strava default must never produce a writing action.
func TestClassifyNeverWritesOverAnExistingTitle(t *testing.T) {
	t.Parallel()

	writingActions := map[Action]struct{}{
		ActionCommuteTemplate: {},
		ActionErrandTemplate:  {},
		ActionLLM:             {},
		ActionLLMIndoor:       {},
	}

	nonDefaultTitles := []string{
		"The Pink Panther Strikes Again",
		"Zur Arbeit",
		"Nach Hause",
		"NBD",
		"",
	}

	// Deliberately contradictory: every flag set at once, so no single tier
	// rule can be the reason the gate holds.
	sportTypes := []string{"Ride", "GravelRide", "VirtualRide", "Run", "Walk", "WeightTraining"}

	cfg := geofencedConfig()
	cfg.ZwiftMode = ZwiftLLMIndoor

	for _, title := range nonDefaultTitles {
		for _, sportType := range sportTypes {
			activity := Activity{
				Name:              title,
				SportType:         sportType,
				Commute:           true,
				Trainer:           true,
				DistanceMeters:    80000,
				MovingTimeSeconds: 12000,
				Start:             atHome,
				End:               atWork,
			}

			got := Classify(activity, cfg)
			if _, writes := writingActions[got.Action]; writes {
				t.Errorf("title %q, sport type %s: action = %v, want a non-writing action",
					title, sportType, got.Action)
			}
		}
	}
}

func TestIsDefaultTitle(t *testing.T) {
	t.Parallel()

	defaults := []string{
		"Morning Ride",
		"Lunch Ride",
		"Afternoon Ride",
		"Evening Ride",
		"Night Ride",
		"Morning Gravel Ride",
		"Afternoon Gravel Ride",
		"Morning Run",
		"Afternoon Walk",
		"Evening Walk",
		"Morning Hike",
		"Lunch Swim",
		"Night Workout",
		"Evening Weight Training",
		// Localized defaults.
		"Radfahrt am Morgen",
		"Gravel-Fahrt am Nachmittag",
		"Lauf am Abend",
		"Spaziergang am Mittag",
		"Schwimmen in der Nacht",
		"Gewichtstraining am Abend",
	}

	for _, title := range defaults {
		if !IsDefaultTitle(title) {
			t.Errorf("IsDefaultTitle(%q) = false, want true", title)
		}
	}

	nonDefaults := []string{
		"",
		"   ",
		"Zur Arbeit",
		"Nach Hause",
		"The Pink Panther Checks Inn",
		"New Bike Day for real",
		"NBD",
		"Morning",
		"Ride",
		"Morning Ride to the bakery",
		"A Morning Ride",
		"morning ride",
		"Morning  Ride",
		"Kellerwinter, Woche 3",
	}

	for _, title := range nonDefaults {
		if IsDefaultTitle(title) {
			t.Errorf("IsDefaultTitle(%q) = true, want false", title)
		}
	}
}

// TestDefaultTitleTableIsComplete pins the size of the generated table so a
// dropped daypart or activity noun shows up as a test failure.
func TestDefaultTitleTableIsComplete(t *testing.T) {
	t.Parallel()

	want := len(englishDayparts)*len(englishActivities) +
		len(germanDayparts)*len(germanActivities)

	if got := len(defaultTitles); got != want {
		t.Errorf("len(defaultTitles) = %d, want %d (duplicate entries?)", got, want)
	}

	for _, daypart := range englishDayparts {
		for _, activity := range englishActivities {
			if title := daypart + " " + activity; !IsDefaultTitle(title) {
				t.Errorf("IsDefaultTitle(%q) = false, want true", title)
			}
		}
	}
	for _, daypart := range germanDayparts {
		for _, activity := range germanActivities {
			if title := activity + " " + daypart; !IsDefaultTitle(title) {
				t.Errorf("IsDefaultTitle(%q) = false, want true", title)
			}
		}
	}
}

func TestGeofenceContains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		geofence Geofence
		point    Point
		want     bool
	}{
		{"inside", testHome, Point{Lat: 0.001, Lon: 0}, true},
		{"outside", testHome, Point{Lat: 0.01, Lon: 0}, false},
		{"far away", testHome, *elsewhere, false},
		{"zero geofence matches nothing", Geofence{}, Point{}, false},
		{"zero radius matches nothing", Geofence{Center: Point{Lat: 0, Lon: 0}}, Point{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.geofence.contains(tt.point); got != tt.want {
				t.Errorf("contains(%v) = %v, want %v", tt.point, got, tt.want)
			}
		})
	}
}

func TestDistanceMeters(t *testing.T) {
	t.Parallel()

	// One degree of latitude is ~111.2 km anywhere on the globe.
	got := distanceMeters(Point{Lat: 0, Lon: 0}, Point{Lat: 1, Lon: 0})
	if math.Abs(got-111195) > 100 {
		t.Errorf("distanceMeters(0,0 -> 1,0) = %.1f m, want ~111195 m", got)
	}

	if got := distanceMeters(Point{Lat: 0.05, Lon: 0}, Point{Lat: 0.05, Lon: 0}); got != 0 {
		t.Errorf("distanceMeters of a point to itself = %v, want 0", got)
	}
}

func TestStringers(t *testing.T) {
	t.Parallel()

	tiers := map[Tier]string{
		TierNone:      "none",
		TierSkip:      "skip",
		TierVirtual:   "virtual",
		TierCommute:   "commute",
		TierErrand:    "errand",
		TierSportRide: "sport_ride",
		Tier(99):      "unknown",
	}
	for tier, want := range tiers {
		if got := tier.String(); got != want {
			t.Errorf("Tier(%d).String() = %q, want %q", int(tier), got, want)
		}
	}

	actions := map[Action]string{
		ActionSkip:            "skip",
		ActionCommuteTemplate: "commute_template",
		ActionErrandTemplate:  "errand_template",
		ActionLLM:             "llm",
		ActionLLMIndoor:       "llm_indoor",
		Action(99):            "unknown",
	}
	for action, want := range actions {
		if got := action.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", int(action), got, want)
		}
	}

	directions := map[Direction]string{
		DirectionNone:   "none",
		DirectionToWork: "to_work",
		DirectionToHome: "to_home",
		Direction(99):   "unknown",
	}
	for direction, want := range directions {
		if got := direction.String(); got != want {
			t.Errorf("Direction(%d).String() = %q, want %q", int(direction), got, want)
		}
	}
}

// The machine-title gate. Xert renames sport rides shortly after upload, so a
// titled activity has not necessarily been named by the athlete.
func TestMachineTitleGate(t *testing.T) {
	t.Parallel()

	sportRide := func(name string) Activity {
		return Activity{
			Name:              name,
			SportType:         "GravelRide",
			DistanceMeters:    67638.5,
			MovingTimeSeconds: 10876,
		}
	}

	tests := []struct {
		name  string
		title string
		want  Action
		why   string
	}{
		{
			name:  "Xert focus title is renamable",
			title: "Difficult Mixed Breakaway Specialist Ride",
			want:  ActionLLM,
			why:   "the acceptance case: a machine title, not the athlete's",
		},
		{
			name:  "Xert focus title without the Mixed modifier",
			title: "Hard Climber Ride",
			want:  ActionLLM,
			why:   "same construction, no modifier",
		},
		{
			name:  "a human title is never overwritten",
			title: "The Pink Panther Checks Inn",
			want:  ActionSkip,
			why:   "the acceptance case on the other side of the gate",
		},
		{
			name:  "an ActivityFix commute title is left alone",
			title: "Zur Arbeit",
			want:  ActionSkip,
			why:   "machine-written, but already the right title for the ride",
		},
		{
			name:  "a title merely shaped like Xert's is not enough",
			title: "Sunday Morning Ride",
			want:  ActionSkip,
			why:   "ends in Ride, but neither word is Xert vocabulary",
		},
		{
			name:  "a Strava default is renamable as before",
			title: "Afternoon Gravel Ride",
			want:  ActionLLM,
			why:   "the gate's original job still works",
		},
	}

	cfg := Config{MachineTitles: DefaultMachineTitles()}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Classify(sportRide(tt.title), cfg)
			if got.Action != tt.want {
				t.Errorf("Classify(%q).Action = %v, want %v — %s (reason: %s)",
					tt.title, got.Action, tt.want, tt.why, got.Reason)
			}
		})
	}
}

// Without configured patterns nothing is renamable but a Strava default, so a
// deployment that says nothing about machine titles keeps the old behavior.
func TestMachineTitleGateIsClosedByDefault(t *testing.T) {
	t.Parallel()

	activity := Activity{
		Name:           "Difficult Mixed Breakaway Specialist Ride",
		SportType:      "GravelRide",
		DistanceMeters: 67638.5,
	}

	if got := Classify(activity, Config{}); got.Action != ActionSkip {
		t.Errorf("Classify with no patterns = %v, want ActionSkip", got.Action)
	}
}

func TestMachineTitlesRejectsABadPattern(t *testing.T) {
	t.Parallel()

	if _, err := NewMachineTitles([]string{"("}); err == nil {
		t.Error("NewMachineTitles with an unclosed group = nil error, want error")
	}
}

// Blank entries are ignored rather than compiled into a pattern that matches
// everything, which is what an empty regex would do.
func TestMachineTitlesIgnoresBlankPatterns(t *testing.T) {
	t.Parallel()

	titles, err := NewMachineTitles([]string{"", "   "})
	if err != nil {
		t.Fatalf("NewMachineTitles: %v", err)
	}

	if titles.Matches("The Pink Panther Checks Inn") {
		t.Error("a blank pattern matched a human title")
	}
}

// A configured pattern matches the whole title or not at all.
//
// A regexp matches anywhere by default, so an unanchored "Ride" would have
// accepted "The Pink Panther Ride" — a human title, overwritten, from
// configuration alone. That is the failure this gate exists to prevent.
func TestMachineTitlePatternsMatchTheWholeTitle(t *testing.T) {
	t.Parallel()

	titles, err := NewMachineTitles([]string{"Ride"})
	if err != nil {
		t.Fatalf("NewMachineTitles: %v", err)
	}

	tests := []struct {
		title string
		want  bool
		why   string
	}{
		{title: "Ride", want: true, why: "the whole title is the pattern"},
		{title: "The Pink Panther Ride", want: false, why: "a human title ending in the pattern"},
		{title: "Ride to the Musterberg", want: false, why: "a human title starting with it"},
		{title: "Afternoon Ride Home", want: false, why: "the pattern in the middle"},
	}

	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			t.Parallel()

			if got := titles.Matches(tt.title); got != tt.want {
				t.Errorf("Matches(%q) = %v, want %v — %s", tt.title, got, tt.want, tt.why)
			}
		})
	}
}

// Anchoring is applied on top of whatever the pattern says, so an already
// anchored pattern keeps matching exactly what it matched before.
func TestMachineTitlePatternsToleratePreAnchoring(t *testing.T) {
	t.Parallel()

	titles, err := NewMachineTitles([]string{`^Hard \w+ Ride$`})
	if err != nil {
		t.Fatalf("NewMachineTitles: %v", err)
	}

	if !titles.Matches("Hard Climber Ride") {
		t.Error("an already-anchored pattern stopped matching its own title")
	}

	if titles.Matches("Very Hard Climber Ride") {
		t.Error("an already-anchored pattern matched a longer title")
	}
}

// The multiline flag makes ^ and $ mean line boundaries, so anchoring with
// them would still admit a multi-line title. \A and \z do not.
func TestMachineTitlePatternsResistMultilineFlags(t *testing.T) {
	t.Parallel()

	titles, err := NewMachineTitles([]string{`(?m)^Ride$`})
	if err != nil {
		t.Fatalf("NewMachineTitles: %v", err)
	}

	if titles.Matches("The Pink Panther Checks Inn\nRide") {
		t.Error("a multiline pattern matched a title whose first line is human-written")
	}
}
