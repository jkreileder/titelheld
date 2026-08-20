package app

import (
	"context"
	"fmt"

	"github.com/jkreileder/titelheld/internal/strava"
)

// boundTokens resolves the athlete when none is configured.
//
// STRAVA_ATHLETE_ID is optional, and unset is the deployed configuration: the
// service binds to whoever completes the one-time OAuth flow and refuses
// everyone afterwards. The webhook copes with that on its own, because every
// event names its athlete. The sweep does not — nothing tells it whose queue it
// is draining, so it would ask the store for athlete 0, a document that never
// exists, and fail on every activity of every sweep, forever, with the entries
// staying queued and nothing ever explaining why.
//
// So the lookup falls back to the single bound athlete. AnyToken returns a
// token only when there is exactly one, which is the same rule the OAuth
// callback binds by; more than one, or none, is [strava.ErrTokenNotFound] and
// the sweep reports it as a missing token rather than guessing.
//
// The resolution is per call rather than at startup on purpose. The service
// starts before anyone has authorized — that is the whole point of the
// bootstrap route — so an athlete resolved once at construction would be no
// athlete at all.
type boundTokens struct {
	store boundStore
}

func (b boundTokens) Load(ctx context.Context, athleteID int64) (strava.Token, error) {
	if athleteID != 0 {
		return b.store.Load(ctx, athleteID)
	}

	token, err := b.store.AnyToken(ctx)
	if err != nil {
		return strava.Token{}, fmt.Errorf(
			"%w: no athlete is configured and none is bound", strava.ErrTokenNotFound)
	}

	return token, nil
}

func (b boundTokens) Save(ctx context.Context, token strava.Token) error {
	return b.store.Save(ctx, token)
}
