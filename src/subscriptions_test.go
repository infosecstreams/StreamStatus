package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/nicklaw5/helix/v2"
)

const testCallback = "https://example.test/webhook/callbacks"

func sub(id, typ, broadcaster, status, callback string) helix.EventSubSubscription {
	return helix.EventSubSubscription{
		ID: id, Type: typ, Status: status,
		Condition: helix.EventSubCondition{BroadcasterUserID: broadcaster},
		Transport: helix.EventSubTransport{Method: "webhook", Callback: callback},
	}
}

func TestPlanSubscriptions(t *testing.T) {
	existing := []helix.EventSubSubscription{
		sub("a1", "stream.online", "1", "enabled", testCallback),
		sub("a2", "stream.offline", "1", "enabled", testCallback),
		sub("a3", "channel.update", "1", "webhook_callback_verification_pending", testCallback),
		// Disabled by Twitch after failed deliveries: replace it.
		sub("b1", "stream.online", "2", "notification_failures_exceeded", testCallback),
		sub("b2", "stream.offline", "2", "enabled", testCallback),
		// Another app/callback's subscription: not ours to touch.
		sub("x1", "stream.online", "3", "enabled", "https://elsewhere.test/cb"),
		// A streamer no longer in the CSVs: left alone.
		sub("z1", "stream.online", "99", "enabled", testCallback),
	}
	plan := planSubscriptions([]string{"1", "2", "3"}, existing, testCallback, "s3cret")

	if len(plan.remove) != 1 || plan.remove[0].ID != "b1" {
		t.Errorf("remove = %+v, want only b1", plan.remove)
	}
	var got []string
	for _, c := range plan.create {
		got = append(got, c.Type+"/"+c.Condition.BroadcasterUserID)
		if c.Transport.Callback != testCallback || c.Transport.Secret != "s3cret" || c.Transport.Method != "webhook" {
			t.Errorf("bad transport %+v", c.Transport)
		}
		if c.Type == "channel.update" && c.Version != "2" {
			t.Errorf("channel.update version %q, want 2", c.Version)
		}
	}
	want := "stream.online/2,channel.update/2,stream.online/3,stream.offline/3,channel.update/3"
	if strings.Join(got, ",") != want {
		t.Errorf("create = %s\nwant     %s", strings.Join(got, ","), want)
	}
}

// fakeSubs is a scripted Helix for subscription management.
type fakeSubs struct {
	users    map[string]string // login -> id
	existing []helix.EventSubSubscription
	budget   int
	created  []helix.EventSubSubscription
	removed  []string
	failType string
}

func (f *fakeSubs) GetUsers(p *helix.UsersParams) (*helix.UsersResponse, error) {
	r := &helix.UsersResponse{}
	r.StatusCode = 200
	for _, l := range p.Logins {
		if id, ok := f.users[strings.ToLower(l)]; ok {
			r.Data.Users = append(r.Data.Users, helix.User{ID: id, Login: strings.ToLower(l)})
		}
	}
	return r, nil
}

func (f *fakeSubs) GetEventSubSubscriptions(p *helix.EventSubSubscriptionsParams) (*helix.EventSubSubscriptionsResponse, error) {
	r := &helix.EventSubSubscriptionsResponse{}
	r.StatusCode = 200
	// Two pages to exercise pagination.
	half := len(f.existing) / 2
	if p.After == "" {
		r.Data.EventSubSubscriptions = f.existing[:half]
		r.Data.Pagination.Cursor = "page2"
	} else {
		r.Data.EventSubSubscriptions = f.existing[half:]
	}
	r.Data.MaxTotalCost, r.Data.TotalCost = 10000, 10000-f.budget
	return r, nil
}

func (f *fakeSubs) CreateEventSubSubscription(p *helix.EventSubSubscription) (*helix.EventSubSubscriptionsResponse, error) {
	r := &helix.EventSubSubscriptionsResponse{}
	if p.Type == f.failType {
		r.StatusCode, r.Error, r.ErrorMessage = 409, "Conflict", "subscription already exists"
		return r, nil
	}
	r.StatusCode = 202
	f.created = append(f.created, *p)
	return r, nil
}

func (f *fakeSubs) RemoveEventSubSubscription(id string) (*helix.RemoveEventSubSubscriptionParamsResponse, error) {
	r := &helix.RemoveEventSubSubscriptionParamsResponse{}
	r.StatusCode = 204
	f.removed = append(f.removed, id)
	return r, nil
}

func TestSyncSubscriptions(t *testing.T) {
	noSleep(t)
	s, _ := newGitFixture(t, []string{offlineLine("alpha")}, nil, "alpha,\nRenamedGuy,\n", "beta,\n")
	f := &fakeSubs{
		users: map[string]string{"alpha": "1", "beta": "2"},
		existing: []helix.EventSubSubscription{
			sub("a1", "stream.online", "1", "enabled", testCallback),
			sub("a2", "stream.offline", "1", "authorization_revoked", testCallback),
		},
		budget: 1000,
	}
	alerts := &fakeAlerts{}
	s.subs, s.callbackURL, s.secret, s.alerts = f, testCallback, "s3cret", alerts

	s.syncSubscriptions()

	if len(f.removed) != 1 || f.removed[0] != "a2" {
		t.Errorf("removed %v, want [a2]", f.removed)
	}
	// alpha: offline + channel.update; beta (inactive): all three.
	if len(f.created) != 5 {
		t.Errorf("created %d subscriptions, want 5: %+v", len(f.created), f.created)
	}
	waitAlerts(t, alerts, 1)
	msg := alerts.all()[0].message
	if !strings.Contains(msg, "created 5") || !strings.Contains(msg, "RenamedGuy") {
		t.Errorf("alert %q should report creations and the unknown login", msg)
	}
}

func TestSyncSubscriptionsStopsAtCostLimit(t *testing.T) {
	noSleep(t)
	s, _ := newGitFixture(t, nil, nil, "alpha,\nbeta,\n", "")
	f := &fakeSubs{users: map[string]string{"alpha": "1", "beta": "2"}, budget: 4}
	alerts := &fakeAlerts{}
	s.subs, s.callbackURL, s.alerts = f, testCallback, alerts

	s.syncSubscriptions()

	if len(f.created) != 4 {
		t.Errorf("created %d, want 4 (the remaining budget)", len(f.created))
	}
	waitAlerts(t, alerts, 1)
	if msg := alerts.all()[0].message; !strings.Contains(msg, "cost limit") {
		t.Errorf("alert %q should mention the cost limit", msg)
	}
}

func TestSyncSubscriptionsDisabledWithoutCallback(t *testing.T) {
	f := &fakeSubs{}
	s := &StreamersRepo{subs: f}
	s.syncSubscriptions() // must not touch the (unconfigured) repo or Helix
	if len(f.created)+len(f.removed) != 0 {
		t.Error("sync ran without SS_CALLBACK_URL")
	}
}

func TestUserIDsBatches(t *testing.T) {
	users := map[string]string{}
	var logins []string
	for i := 0; i < 230; i++ {
		l := fmt.Sprintf("u%d", i)
		logins = append(logins, l)
		if i != 7 {
			users[l] = fmt.Sprint(1000 + i)
		}
	}
	s := &StreamersRepo{subs: &fakeSubs{users: users}}
	ids, missing, err := s.userIDs(logins)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 229 || len(missing) != 1 || missing[0] != "u7" {
		t.Errorf("got %d ids, missing %v; want 229 and [u7]", len(ids), missing)
	}
}

func waitAlerts(t *testing.T, f *fakeAlerts, n int) {
	t.Helper()
	for i := 0; i < 200 && len(f.all()) < n; i++ {
		realSleep(5)
	}
	if got := len(f.all()); got != n {
		t.Fatalf("got %d alerts, want %d: %+v", got, n, f.all())
	}
}
