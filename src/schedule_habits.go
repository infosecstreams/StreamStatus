package main

import (
	"sort"
	"time"
)

// Past streams (from bot-live-data's database: recorded live, plus the
// one-time backfill imported from the SullyGnome/TwitchTracker scrape)
// feed two things:
//
//   - estimated track records: each streamer's current *recurring*
//     segments projected back 13 weeks and judged against their past
//     streams with the same ±adherenceTolerance rule as observed outcomes.
//     Real observed outcomes replace estimates for the same slot.
//   - habits: weekly UTC slots a streamer usually streams in, schedule or
//     not ("Tuesdays ~18:00 UTC, 11 of the last 13 weeks").
//
// Both are recomputed after every schedule refresh, off the worker.

const (
	historyWindow  = 90 * 24 * time.Hour
	habitMinWeeks  = 4   // a slot needs a stream in this many weeks
	habitMinShare  = 0.5 // and in at least this share of the weeks
	habitSlotRound = 15 * time.Minute
)

// pastStream is one stream a streamer did.
type pastStream struct {
	Start  time.Time
	Length time.Duration
}

// pastStreamsFunc returns a streamer's streams since a time, oldest first;
// ok is false if history is unavailable.
type pastStreamsFunc func(id string, since time.Time) ([]pastStream, bool)

// recordedPastStreams adapts bot-live-data's pastStreams.
func (s *StreamersRepo) recordedPastStreams(id string, since time.Time) ([]pastStream, bool) {
	rec, ok := s.pastStreams(id, since)
	if !ok {
		return nil, false
	}
	out := make([]pastStream, len(rec))
	for i, r := range rec {
		out[i] = pastStream{Start: r.Start, Length: r.Length}
	}
	return out, true
}

// updateEstimates recomputes estimated outcomes and habits from past
// streams and the current recurring segments.
func (s *StreamersRepo) updateEstimates() {
	st := s.schedule
	if st == nil || st.adherence == nil {
		return
	}
	past := st.past
	if past == nil {
		past = s.recordedPastStreams
	}
	now := timeNow()
	st.mu.RLock()
	segs := make(map[string][]scheduledSegment, len(st.roster))
	for id := range st.roster {
		if b := st.byID[id]; b != nil {
			segs[id] = b.segments
		} else {
			segs[id] = nil
		}
	}
	st.mu.RUnlock()

	est := map[string][]adherenceOutcome{}
	hab := map[string][]habitSlot{}
	for id, sg := range segs {
		streams, ok := past(id, now.Add(-historyWindow))
		if !ok {
			continue
		}
		if o := estimateOutcomes(sg, streams, now); len(o) > 0 {
			est[id] = o
		}
		if h := habits(streams, now); len(h) > 0 {
			hab[id] = h
		}
	}
	st.adherence.setEstimated(est)
	st.mu.Lock()
	st.habits = hab
	st.mu.Unlock()
}

// estimateOutcomes projects each recurring segment back week by week over
// the history window and judges each past occurrence against streams: kept
// if a stream overlapped start ± adherenceTolerance. Occurrences are only
// judged once their window has closed.
func estimateOutcomes(segs []scheduledSegment, streams []pastStream, now time.Time) []adherenceOutcome {
	week := 7 * 24 * time.Hour
	seen := map[int64]bool{}
	var out []adherenceOutcome
	for _, g := range segs {
		if !g.Recurring {
			continue
		}
		for k := 1; ; k++ {
			occ := g.Start.Add(-time.Duration(k) * week)
			if now.Sub(occ) > historyWindow {
				break
			}
			if occ.Add(adherenceTolerance).After(now) || seen[occ.Unix()] {
				continue
			}
			seen[occ.Unix()] = true
			o := adherenceOutcome{SegmentID: g.ID, Start: occ, Estimated: true}
			from, to := occ.Add(-adherenceTolerance), occ.Add(adherenceTolerance)
			for _, p := range streams {
				if p.Start.Before(to) && p.Start.Add(p.Length).After(from) {
					o.Kept, o.LiveAt = true, p.Start
					break
				}
			}
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// habitSlot is a weekly UTC time a streamer usually streams at.
type habitSlot struct {
	Weekday time.Weekday
	Minute  int // minutes after UTC midnight
	Weeks   int // weeks with a stream starting within ±adherenceTolerance
	Of      int // weeks of history
}

// streamHabits returns streamer id's usual weekly slots, most regular
// first, as of the last schedule refresh.
func (s *StreamersRepo) streamHabits(id string) []habitSlot {
	if s.schedule == nil {
		return nil
	}
	s.schedule.mu.RLock()
	defer s.schedule.mu.RUnlock()
	return s.schedule.habits[id]
}

// habits finds weekly slots: for each candidate minute-of-week (every
// stream start), count the weeks of the history window that have a stream
// starting within adherenceTolerance of it. Slots in at least habitMinWeeks
// weeks and habitMinShare of them are kept, greedily, so that no two are
// closer than twice the tolerance.
func habits(streams []pastStream, now time.Time) []habitSlot {
	const weekMin = 7 * 24 * 60
	tol := int(adherenceTolerance / time.Minute)
	start := now.Add(-historyWindow)
	weekOf := func(t time.Time) int { return int(t.Sub(start) / (7 * 24 * time.Hour)) }
	mow := func(t time.Time) int {
		t = t.UTC()
		return int(t.Weekday())*24*60 + t.Hour()*60 + t.Minute()
	}
	dist := func(a, b int) int {
		d := (a - b + weekMin) % weekMin
		return min(d, weekMin-d)
	}
	var recent []pastStream
	for _, p := range streams {
		if !p.Start.Before(start) && p.Start.Before(now) {
			recent = append(recent, p)
		}
	}
	type cand struct {
		minute, weeks, of int
	}
	var cands []cand
	for _, c := range recent {
		m := mow(c.Start)
		weeks := map[int]bool{}
		var offsets []int
		for _, p := range recent {
			if d := mow(p.Start); dist(d, m) <= tol {
				weeks[weekOf(p.Start)] = true
				off := (d - m + weekMin) % weekMin
				if off > weekMin/2 {
					off -= weekMin
				}
				offsets = append(offsets, off)
			}
		}
		sort.Ints(offsets)
		center := (m + offsets[len(offsets)/2] + weekMin) % weekMin
		// Occurrences of this minute-of-week inside the window.
		of := 0
		first := start.Truncate(24 * time.Hour).Add(-7 * 24 * time.Hour)
		for t := first; t.Before(now); t = t.Add(24 * time.Hour) {
			occ := t.Add(time.Duration(center%(24*60)) * time.Minute)
			if int(occ.Weekday()) == center/(24*60) && !occ.Before(start) && occ.Before(now) {
				of++
			}
		}
		cands = append(cands, cand{center, len(weeks), of})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].weeks != cands[j].weeks {
			return cands[i].weeks > cands[j].weeks
		}
		return cands[i].minute < cands[j].minute
	})
	var out []habitSlot
	for _, c := range cands {
		if c.weeks < habitMinWeeks || c.of == 0 || float64(c.weeks) < habitMinShare*float64(c.of) {
			continue
		}
		clash := false
		for _, h := range out {
			if dist(int(h.Weekday)*24*60+h.Minute, c.minute) <= 2*tol {
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		minute := c.minute % (24 * 60)
		r := int(habitSlotRound / time.Minute)
		minute = (minute + r/2) / r * r % (24 * 60)
		out = append(out, habitSlot{Weekday: time.Weekday(c.minute / (24 * 60)), Minute: minute, Weeks: min(c.weeks, c.of), Of: c.of})
	}
	return out
}

// nextHabitAt returns the next start of one of a streamer's habit slots.
func nextHabitAt(slots []habitSlot, now time.Time) (time.Time, bool) {
	var next time.Time
	day := now.UTC().Truncate(24 * time.Hour)
	for _, h := range slots {
		for d := 0; d <= 7; d++ {
			t := day.Add(time.Duration(d)*24*time.Hour + time.Duration(h.Minute)*time.Minute)
			if t.Weekday() != h.Weekday || !t.After(now) {
				continue
			}
			if next.IsZero() || t.Before(next) {
				next = t
			}
			break
		}
	}
	return next, !next.IsZero()
}
