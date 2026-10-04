package proxy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goodtekxyz/openllms/internal/proxy"
	"github.com/goodtekxyz/openllms/internal/store"
	"github.com/google/uuid"
)

type cooldownReadFailure struct {
	*memRouteStore
	failure error
}

func (s cooldownReadFailure) EarliestRouteCooldown(context.Context, uuid.UUID) (time.Time, bool, error) {
	return time.Time{}, false, s.failure
}

func TestCooldownReadFailureRejectsDispatch(t *testing.T) {
	id := uuid.New()
	want := errors.New("synthetic cooldown read failure")
	st := cooldownReadFailure{&memRouteStore{accounts: []store.Account{{ID: id}}, cool: map[uuid.UUID]time.Time{id: time.Now().Add(time.Minute)}}, want}
	engine := &proxy.Engine{Store: st}
	result, err := engine.ChatCompletions(context.Background(), uuid.New(), &store.Route{ID: uuid.New(), Strategy: "sequential"}, []byte(`{"model":"synthetic","messages":[]}`), proxy.DispatchMeta{})
	if !errors.Is(err, want) || result != nil {
		t.Fatalf("read failure became a response: result=%v err=%v", result, err)
	}
}
