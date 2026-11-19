package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// subscriptionsClient is the slice of Helix used to manage EventSub
// subscriptions; *helix.Client satisfies it.
type subscriptionsClient interface {
	GetUsers(params *helix.UsersParams) (*helix.UsersResponse, error)
	GetEventSubSubscriptions(params *helix.EventSubSubscriptionsParams) (*helix.EventSubSubscriptionsResponse, error)
	CreateEventSubSubscription(payload *helix.EventSubSubscription) (*helix.EventSubSubscriptionsResponse, error)
	RemoveEventSubSubscription(id string) (*helix.RemoveEventSubSubscriptionParamsResponse, error)
}

// wantedSubscriptions are the EventSub types (and versions) every streamer
// needs. channel.update feeds the pre-live marker.
var wantedSubscriptions = []struct{ typ, version string }{
	{helix.EventSubTypeStreamOnline, "1"},
	{helix.EventSubTypeStreamOffline, "1"},
	{helix.EventSubTypeChannelUpdate, "2"},
}

// subscriptionSyncInterval is how often subscriptions are re-checked, so
// streamers added to the CSVs get subscribed without a restart.
const subscriptionSyncInterval = 24 * time.Hour

// subCreatePause spaces out subscription creation; swapped out in tests.
var subCreatePause = 100 * time.Millisecond

// keepSubscription reports whether an existing subscription with this
// status still works (or is about to).
func keepSubscription(status string) bool {
	return status == helix.EventSubStatusEnabled || status == "webhook_callback_verification_pending"
}

// startSubscriptionSync queues a subscription sync now and then daily.
func (s *StreamersRepo) startSubscriptionSync() {
	s.enqueue(statusJob{kind: jobSyncSubscriptions})
	go func() {
		t := time.NewTicker(subscriptionSyncInterval)
		defer t.Stop()
		for range t.C {
			s.enqueue(statusJob{kind: jobSyncSubscriptions})
		}
	}()
}

// subscriptionPlan is what a sync will do.
type subscriptionPlan struct {
	create []helix.EventSubSubscription
	remove []helix.EventSubSubscription
}

// planSubscriptions compares the subscriptions that exist for our callback
// with the ones every broadcaster ID should have. Broken subscriptions are
// removed and recreated; subscriptions for broadcasters not in ids are left
// alone.
func planSubscriptions(ids []string, existing []helix.EventSubSubscription, callback, secret string) subscriptionPlan {
	return planSubscriptionsWith(ids, existing, callback, secret, false)
}

// raidSubscriptions are channel.raid subscriptions for raids out of
// (from) and into (to) each streamer; they feed raid stats (stats.go) and
// are only made when live state is on.
var raidSubscriptions = []struct {
	version string
	cond    func(id string) helix.EventSubCondition
}{
	{"1", func(id string) helix.EventSubCondition { return helix.EventSubCondition{FromBroadcasterUserID: id} }},
	{"1", func(id string) helix.EventSubCondition { return helix.EventSubCondition{ToBroadcasterUserID: id} }},
}

// subscriptionKey identifies a subscription by type and condition.
func subscriptionKey(typ string, c helix.EventSubCondition) string {
	return typ + "/" + c.BroadcasterUserID + "/" + c.FromBroadcasterUserID + "/" + c.ToBroadcasterUserID
}

// planSubscriptionsWith is planSubscriptions, adding the raid
// subscriptions if raids is set.
func planSubscriptionsWith(ids []string, existing []helix.EventSubSubscription, callback, secret string, raids bool) subscriptionPlan {
	var plan subscriptionPlan
	have := map[string]bool{}
	for _, sub := range existing {
		if sub.Transport.Callback != callback {
			continue
		}
		if !keepSubscription(sub.Status) {
			plan.remove = append(plan.remove, sub)
			continue
		}
		have[subscriptionKey(sub.Type, sub.Condition)] = true
	}
	want := func(typ, version string, cond helix.EventSubCondition) {
		key := subscriptionKey(typ, cond)
		if have[key] {
			return
		}
		have[key] = true
		plan.create = append(plan.create, helix.EventSubSubscription{
			Type:      typ,
			Version:   version,
			Condition: cond,
			Transport: helix.EventSubTransport{Method: "webhook", Callback: callback, Secret: secret},
		})
	}
	for _, id := range ids {
		for _, w := range wantedSubscriptions {
			want(w.typ, w.version, helix.EventSubCondition{BroadcasterUserID: id})
		}
		if raids {
			for _, r := range raidSubscriptions {
				want(helix.EventSubTypeChannelRaid, r.version, r.cond(id))
			}
		}
	}
	return plan
}

// syncSubscriptions makes sure every streamer in both CSVs has the
// subscriptions in wantedSubscriptions. Callers must hold s.mutex.
func (s *StreamersRepo) syncSubscriptions() {
	if s.subs == nil || s.callbackURL == "" {
		return
	}
	log.Info("subscriptions: syncing")
	if err := s.getRepo(); err != nil {
		log.Errorf("subscriptions: syncing repo failed, skipping: %s", err)
		return
	}
	logins, err := csvNames(s.activeCsvPath)
	if err != nil {
		log.Errorf("subscriptions: reading %s failed, skipping: %s", s.activeCsvPath, err)
		return
	}
	if inactive, err := csvNames(s.inactiveCsvPath); err == nil {
		logins = append(logins, inactive...)
	}

	ids, missing, err := s.userIDs(logins)
	if err != nil {
		log.Errorf("subscriptions: looking up user IDs failed, skipping: %s", err)
		return
	}
	existing, budget, err := s.listSubscriptions()
	if err != nil {
		log.Errorf("subscriptions: listing failed, skipping: %s", err)
		return
	}

	plan := planSubscriptionsWith(ids, existing, s.callbackURL, s.secret, s.live != nil)
	var removed, created, failed []string
	for _, sub := range plan.remove {
		desc := fmt.Sprintf("%s %s (%s)", sub.Type, sub.Condition.BroadcasterUserID, sub.Status)
		resp, err := s.subs.RemoveEventSubSubscription(sub.ID)
		if err == nil {
			err = helixStatus(resp.ResponseCommon, 204)
		}
		if err != nil {
			failed = append(failed, "remove "+desc+": "+err.Error())
			continue
		}
		removed = append(removed, desc)
	}
	for i, sub := range plan.create {
		desc := sub.Type + " " + sub.Condition.BroadcasterUserID
		if c := sub.Condition; c.FromBroadcasterUserID != "" {
			desc += "from " + c.FromBroadcasterUserID
		} else if c.ToBroadcasterUserID != "" {
			desc += "to " + c.ToBroadcasterUserID
		}
		if budget >= 0 && i >= budget {
			failed = append(failed, fmt.Sprintf("create %s and %d more: EventSub cost limit reached", desc, len(plan.create)-i-1))
			break
		}
		sub := sub
		resp, err := s.subs.CreateEventSubSubscription(&sub)
		if err == nil {
			err = helixStatus(resp.ResponseCommon, 202)
		}
		if err != nil {
			failed = append(failed, "create "+desc+": "+err.Error())
		} else {
			created = append(created, desc)
		}
		sleep(subCreatePause)
	}

	log.Infof("subscriptions: %d streamers, %d existing, %d created, %d removed, %d failed, %d logins not found",
		len(ids), len(existing), len(created), len(removed), len(failed), len(missing))
	if len(created)+len(removed)+len(failed)+len(missing) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "created %d, removed %d broken, %d failed\n", len(created), len(removed), len(failed))
	if len(removed) > 0 {
		fmt.Fprintf(&b, "removed: %s\n", strings.Join(removed, ", "))
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "Twitch logins not found (renamed or deleted?): %s\n", strings.Join(missing, ", "))
	}
	for _, f := range failed {
		fmt.Fprintf(&b, "%s\n", f)
	}
	s.alert("EventSub subscriptions updated", b.String())
	if len(failed) > 0 {
		log.Errorf("subscriptions: %d operations failed, first: %s", len(failed), failed[0])
	}
}

// userIDs resolves logins to Twitch user IDs, 100 at a time. missing lists
// the logins Twitch didn't return.
func (s *StreamersRepo) userIDs(logins []string) (ids, missing []string, err error) {
	found := map[string]string{}
	for start := 0; start < len(logins); start += helixLoginBatch {
		end := min(start+helixLoginBatch, len(logins))
		resp, err := s.subs.GetUsers(&helix.UsersParams{Logins: logins[start:end]})
		if err != nil {
			return nil, nil, err
		}
		if err := helixError(resp.ResponseCommon); err != nil {
			return nil, nil, err
		}
		for _, u := range resp.Data.Users {
			found[strings.ToLower(u.Login)] = u.ID
		}
	}
	for _, l := range logins {
		if id, ok := found[strings.ToLower(l)]; ok {
			ids = append(ids, id)
		} else {
			missing = append(missing, l)
		}
	}
	return ids, missing, nil
}

// listSubscriptions returns every subscription for this app and how many
// more cost points are available (-1 if unknown).
func (s *StreamersRepo) listSubscriptions() ([]helix.EventSubSubscription, int, error) {
	var all []helix.EventSubSubscription
	budget := -1
	after := ""
	for page := 0; page < 100; page++ {
		resp, err := s.subs.GetEventSubSubscriptions(&helix.EventSubSubscriptionsParams{After: after})
		if err != nil {
			return nil, 0, err
		}
		if err := helixError(resp.ResponseCommon); err != nil {
			return nil, 0, err
		}
		all = append(all, resp.Data.EventSubSubscriptions...)
		if resp.Data.MaxTotalCost > 0 {
			budget = resp.Data.MaxTotalCost - resp.Data.TotalCost
		}
		after = resp.Data.Pagination.Cursor
		if after == "" {
			return all, budget, nil
		}
	}
	return nil, 0, fmt.Errorf("more than 100 pages of subscriptions")
}

// helixStatus is helixError for endpoints that answer with a status other
// than 200 on success.
func helixStatus(rc helix.ResponseCommon, ok int) error {
	if rc.StatusCode == ok {
		return nil
	}
	return helixError(rc)
}
