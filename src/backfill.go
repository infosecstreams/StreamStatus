package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Backfill: the bot only knows streams since it started recording. To
// have 90 days of history from day one, it imports the streams that
// bot-schedule's tracker scrape (SullyGnome / TwitchTracker) caches in
// stream_history.json, for the time before recording started. Imported
// sessions have source "backfill" and one segment in backfillCategory: the
// trackers give start and length only, and every roster streamer was
// counted on the old site, so backfilled time counts as infosec time.
//
// Once the import has run, the scrape is no longer needed: the database
// keeps the backfill, and streamsSince serves past streams (backfilled
// and recorded) to the schedule code. See DONE-bot-live-data.md.

// backfillCategory is the category of imported time.
const backfillCategory = "Unknown (backfill)"

// countsAsInfosec reports whether time in category counts towards hours,
// streams and viewer figures.
func countsAsInfosec(category string) bool {
	return strings.EqualFold(category, backfillCategory) || contains(VALID_GAMES, category)
}

// defaultBackfillPath is where bot-schedule caches tracker history: next
// to the database.
func defaultBackfillPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "stream_history.json")
}

// backfillStream is one stream in stream_history.json.
type backfillStream struct {
	Start  time.Time     `json:"start"`
	Length time.Duration `json:"length"`
}

type backfillFile struct {
	Streams map[string][]backfillStream `json:"streams"` // by broadcaster ID
}

// liveRecordingSince is when the bot itself started recording (the
// database's creation), ignoring imported history.
func (st *store) liveRecordingSince() (time.Time, error) {
	var v string
	err := st.db.QueryRow(`SELECT value FROM meta WHERE key = 'recording_since'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad recording_since %q", v)
	}
	return time.Unix(n, 0).UTC(), nil
}

// importBackfill adds streams that ended before cutoff (clipped to it) as
// backfilled sessions. Streams overlapping a session already stored are
// skipped, so importing the same file again adds nothing.
func (st *store) importBackfill(streams map[string][]backfillStream, cutoff time.Time) (int, error) {
	tx, err := st.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for id, list := range streams {
		if id == "" {
			continue
		}
		for _, s := range list {
			start, end := s.Start.UTC().Truncate(time.Second), s.Start.Add(s.Length).UTC().Truncate(time.Second)
			if !start.Before(cutoff) {
				continue
			}
			if end.After(cutoff) {
				end = cutoff
			}
			if !end.After(start) {
				continue
			}
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND started_at < ? AND COALESCE(ended_at, last_seen) > ?`,
				id, end.Unix(), start.Unix()).Scan(&n); err != nil {
				return 0, err
			}
			if n > 0 {
				continue
			}
			res, err := tx.Exec(`INSERT INTO sessions (user_id, started_at, ended_at, last_seen, source) VALUES (?, ?, ?, ?, 'backfill')`,
				id, start.Unix(), end.Unix(), end.Unix())
			if err != nil {
				return 0, err
			}
			sid, err := res.LastInsertId()
			if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(`INSERT INTO segments (session_id, user_id, category, started_at, ended_at) VALUES (?, ?, ?, ?, ?)`,
				sid, id, backfillCategory, start.Unix(), end.Unix()); err != nil {
				return 0, err
			}
			added++
		}
	}
	return added, tx.Commit()
}

// importBackfillFile imports path if it changed since the last import.
// A missing file is not an error: there is nothing to backfill.
func (l *liveState) importBackfillFile() {
	if l.backfillPath == "" {
		return
	}
	fi, err := os.Stat(l.backfillPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warnf("backfill: %s", err)
		}
		return
	}
	if fi.ModTime().Equal(l.backfillMtime) {
		return
	}
	data, err := os.ReadFile(l.backfillPath)
	if err != nil {
		log.Warnf("backfill: %s", err)
		return
	}
	var f backfillFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Warnf("backfill: parsing %s: %s", l.backfillPath, err)
		return
	}
	cutoff, err := l.db.liveRecordingSince()
	if err != nil || cutoff.IsZero() {
		log.Warnf("backfill: no recording start in the database: %v", err)
		return
	}
	n, err := l.db.importBackfill(f.Streams, cutoff)
	if err != nil {
		log.Errorf("backfill: importing %s: %s", l.backfillPath, err)
		return
	}
	l.backfillMtime = fi.ModTime()
	if n > 0 {
		log.Infof("backfill: imported %d streams from before %s", n, cutoff.Format(time.RFC3339))
	}
}

// recordedStream is one past stream from the database, backfilled or
// recorded live.
type recordedStream struct {
	Start      time.Time
	Length     time.Duration
	Backfilled bool
}

// streamsSince returns userID's streams that were still running at or
// after since, oldest first. A stream in progress runs until it was last
// seen live. Safe to call from any goroutine.
func (st *store) streamsSince(userID string, since time.Time) ([]recordedStream, error) {
	rows, err := st.db.Query(`SELECT started_at, COALESCE(ended_at, last_seen), source FROM sessions
		WHERE user_id = ? AND COALESCE(ended_at, last_seen) >= ? ORDER BY started_at`, userID, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recordedStream
	for rows.Next() {
		var start, end int64
		var source string
		if err := rows.Scan(&start, &end, &source); err != nil {
			return nil, err
		}
		out = append(out, recordedStream{
			Start:      time.Unix(start, 0).UTC(),
			Length:     time.Duration(end-start) * time.Second,
			Backfilled: source == "backfill",
		})
	}
	return out, rows.Err()
}

// pastStreams is the schedule code's view of a streamer's past streams
// (habits, schedule track records), from the database. ok is false when
// stats are off.
func (s *StreamersRepo) pastStreams(id string, since time.Time) (streams []recordedStream, ok bool) {
	if s.live == nil {
		return nil, false
	}
	streams, err := s.live.db.streamsSince(id, since)
	if err != nil {
		log.Warnf("reading past streams of %s: %s", id, err)
		return nil, false
	}
	return streams, true
}
