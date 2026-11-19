package main

import (
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/nicklaw5/helix/v2"
	log "github.com/sirupsen/logrus"
)

// More recorded detail for stats: raids, followers gained per stream,
// stream tags and topics, and VODs and clips. Computation is in stats.go.

const (
	// raidDedupe merges the two channel.raid deliveries (the raider's
	// "from" and the target's "to" subscription) and a chat USERNOTICE of
	// the same raid.
	raidDedupe = 3 * time.Minute
	// mediaBatch is how many streamers' VODs and clips one media job
	// fetches (two Helix calls each); every streamer is refreshed daily.
	mediaBatch = 25
	// mediaCount is how many VODs and clips are kept per streamer.
	mediaCount = 20
	// sessionFollowersPerRun caps follower snapshots per live refresh.
	sessionFollowersPerRun = 20
)

// --- raids

type raidRecord struct {
	at                          time.Time
	fromID, fromLogin, fromName string
	toID, toLogin, toName       string
	viewers                     int
	source                      string // "eventsub" or "chat"
}

// addRaid stores r unless the same raid was already stored. It reports
// whether r was new.
func (st *store) addRaid(r raidRecord) (bool, error) {
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM raids WHERE from_id = ? AND to_id = ? AND at BETWEEN ? AND ?`,
		r.fromID, r.toID, r.at.Add(-raidDedupe).Unix(), r.at.Add(raidDedupe).Unix()).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	_, err := st.db.Exec(`INSERT INTO raids (at, from_id, from_login, from_name, to_id, to_login, to_name, viewers, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.at.Unix(), r.fromID, strings.ToLower(r.fromLogin), r.fromName,
		r.toID, strings.ToLower(r.toLogin), r.toName, r.viewers, r.source)
	return err == nil, err
}

func (st *store) raidsSince(from time.Time) ([]raidRecord, error) {
	rows, err := st.db.Query(`SELECT at, from_id, from_login, from_name, to_id, to_login, to_name, viewers, source
		FROM raids WHERE at >= ? ORDER BY at`, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []raidRecord
	for rows.Next() {
		var r raidRecord
		var at int64
		if err := rows.Scan(&at, &r.fromID, &r.fromLogin, &r.fromName, &r.toID, &r.toLogin, &r.toName, &r.viewers, &r.source); err != nil {
			return nil, err
		}
		r.at = time.Unix(at, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// recordRaid stores a raid (worker).
func (l *liveState) recordRaid(r raidRecord) {
	if r.fromID == "" || r.toID == "" {
		return
	}
	added, err := l.db.addRaid(r)
	if err != nil {
		log.Errorf("stats: saving raid %s -> %s: %s", r.fromLogin, r.toLogin, err)
		return
	}
	if added {
		log.Infof("raid: %s -> %s with %d viewers (%s)", r.fromLogin, r.toLogin, r.viewers, r.source)
	}
}

// --- followers gained per stream

func (st *store) sessionsNeedingFollowers(now time.Time) (starts, ends []sessionRow, err error) {
	q := func(query string, args ...any) ([]sessionRow, error) {
		rows, err := st.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []sessionRow
		for rows.Next() {
			var r sessionRow
			if err := rows.Scan(&r.id, &r.userID); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}
	if starts, err = q(`SELECT id, user_id FROM sessions WHERE source = 'live' AND ended_at IS NULL
		AND followers_start IS NULL AND started_at >= ? ORDER BY started_at`, now.Add(-6*time.Hour).Unix()); err != nil {
		return nil, nil, err
	}
	ends, err = q(`SELECT id, user_id FROM sessions WHERE source = 'live' AND ended_at IS NOT NULL
		AND followers_end IS NULL AND ended_at >= ? ORDER BY ended_at`, now.Add(-30*time.Minute).Unix())
	return starts, ends, err
}

// snapshotSessionFollowers stores the follower total of streams that just
// started or ended, so followers gained per stream can be shown. It runs
// with the live refresh, so a snapshot is at most liveRefreshInterval late.
func (s *StreamersRepo) snapshotSessionFollowers(now time.Time) {
	if s.live == nil || s.follows == nil {
		return
	}
	starts, ends, err := s.live.db.sessionsNeedingFollowers(now)
	if err != nil {
		log.Errorf("stats: listing streams needing follower totals: %s", err)
		return
	}
	calls := 0
	snap := func(rows []sessionRow, column string) {
		for _, r := range rows {
			if calls >= sessionFollowersPerRun {
				return
			}
			calls++
			resp, err := s.follows.GetChannelFollows(&helix.GetChannelFollowsParams{BroadcasterID: r.userID, First: 1})
			if err == nil {
				err = helixError(resp.ResponseCommon)
			}
			if err != nil {
				log.Warnf("stats: follower total of %s: %s", r.userID, err)
				continue
			}
			if _, err := s.live.db.db.Exec(`UPDATE sessions SET `+column+` = ? WHERE id = ?`, resp.Data.Total, r.id); err != nil {
				log.Errorf("stats: saving follower total of %s: %s", r.userID, err)
				return
			}
			// The daily total too, if today's is missing.
			s.live.db.db.Exec(`INSERT OR IGNORE INTO follower_counts (user_id, day, at, total) VALUES (?, ?, ?, ?)`,
				r.userID, now.UTC().Format(time.DateOnly), now.Unix(), resp.Data.Total)
		}
	}
	snap(ends, "followers_end")
	snap(starts, "followers_start")
}

// --- tags and topics

// tagOpenSession records tags on userID's stream in progress.
func (st *store) tagOpenSession(userID string, tags []string) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sid int64
	err = tx.QueryRow(`SELECT id FROM sessions WHERE user_id = ? AND ended_at IS NULL ORDER BY started_at DESC LIMIT 1`, userID).Scan(&sid)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO session_tags (session_id, tag) VALUES (?, ?)`, sid, t); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// recordTags stores a live stream's tags when they change.
func (l *liveState) recordTags(id string, tags []string) {
	if len(tags) == 0 {
		return
	}
	key := strings.Join(tags, "\x00")
	if l.lastTags == nil {
		l.lastTags = map[string]string{}
	}
	if l.lastTags[id] == key {
		return
	}
	if err := l.db.tagOpenSession(id, tags); err != nil {
		log.Errorf("stats: saving tags of %s: %s", id, err)
		return
	}
	l.lastTags[id] = key
}

// topic is an infosec subject recognised in stream tags and titles.
// Single words match whole words (or whole tags); phrases match anywhere.
type topic struct {
	name     string
	keywords []string
}

var topics = []topic{
	{"CTF", []string{"ctf", "ctfs", "capture the flag", "capturetheflag", "picoctf"}},
	{"Hack The Box", []string{"htb", "hackthebox", "hack the box"}},
	{"TryHackMe", []string{"thm", "tryhackme"}},
	{"Malware analysis", []string{"malware", "malwareanalysis", "malware analysis", "ransomware"}},
	{"Reverse engineering", []string{"reverseengineering", "reverse engineering", "reversing", "ghidra", "ida pro", "binary ninja", "decompiling"}},
	{"Exploit development", []string{"exploit", "exploitation", "exploitdev", "exploit dev", "pwn", "pwning", "buffer overflow", "0day", "rop chain"}},
	{"Bug bounty", []string{"bugbounty", "bug bounty", "hackerone", "bugcrowd", "intigriti"}},
	{"Web security", []string{"websecurity", "web security", "appsec", "xss", "sqli", "sql injection", "burp", "owasp"}},
	{"Pentesting & red team", []string{"pentest", "pentesting", "penetrationtesting", "penetration testing", "redteam", "red team", "red teaming", "active directory"}},
	{"Blue team & DFIR", []string{"blueteam", "blue team", "dfir", "forensics", "incident response", "soc", "threat hunting", "detection engineering", "siem"}},
	{"OSINT", []string{"osint"}},
	{"Cloud security", []string{"cloudsecurity", "cloud security", "aws", "azure", "gcp", "kubernetes", "k8s"}},
	{"Hardware & radio", []string{"hardwarehacking", "hardware hacking", "hardware", "sdr", "radio", "iot", "embedded", "firmware", "flipper"}},
	{"Lockpicking", []string{"lockpicking", "lockpick", "lock picking"}},
	{"Certs & careers", []string{"oscp", "cissp", "comptia", "security+", "certification", "certifications", "career", "careers", "interview"}},
	{"Programming", []string{"programming", "coding", "golang", "rust", "python", "softwaredevelopment", "gamedev", "devops"}},
	{"AI security", []string{"ai", "llm", "llms", "prompt injection", "machinelearning", "machine learning"}},
	{"Privacy", []string{"privacy", "opsec"}},
	{"News & talk", []string{"news", "podcast", "talk show", "infosecnews"}},
}

// normaliseText lowercases s and turns everything but letters, digits
// and '+' into single spaces, padded with a space on each side.
func normaliseText(s string) string {
	var b strings.Builder
	b.WriteByte(' ')
	space := true
	for _, r := range strings.ToLower(s) {
		if r == '+' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	if !space {
		b.WriteByte(' ')
	}
	return b.String()
}

// streamTopics returns the topics found in a stream's tags and titles,
// in the order of topics.
func streamTopics(tags, titles []string) []string {
	var parts []string
	for _, t := range tags {
		parts = append(parts, t, strings.ReplaceAll(t, " ", ""))
	}
	parts = append(parts, titles...)
	text := normaliseText(strings.Join(parts, " | "))
	var out []string
	for _, tp := range topics {
		for _, kw := range tp.keywords {
			if strings.Contains(text, " "+kw+" ") {
				out = append(out, tp.name)
				break
			}
		}
	}
	return out
}

// --- VODs and clips

// mediaClient is the slice of Helix used for VODs and clips;
// *helix.Client satisfies it.
type mediaClient interface {
	GetVideos(params *helix.VideosParams) (*helix.VideosResponse, error)
	GetClips(params *helix.ClipsParams) (*helix.ClipsResponse, error)
}

// mediaItem is a VOD or clip in the stats.
type mediaItem struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	URL          string  `json:"url"`
	ThumbnailURL string  `json:"thumbnail_url,omitempty"`
	Views        int     `json:"views"`
	DurationS    float64 `json:"duration_s"`
	CreatedAt    string  `json:"created_at"`
	CreatorName  string  `json:"creator_name,omitempty"` // clips
	// Streamer fields are set in community lists.
	StreamerID    string `json:"streamer_id,omitempty"`
	StreamerLogin string `json:"streamer_login,omitempty"`
}

func parseHelixTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t.UTC()
}

// replaceMedia stores userID's latest VODs and clips and marks today's
// fetch done.
func (st *store) replaceMedia(userID string, videos []helix.Video, clips []helix.Clip, now time.Time) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if videos != nil {
		if _, err := tx.Exec(`DELETE FROM videos WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, v := range videos {
			d, _ := time.ParseDuration(v.Duration)
			if _, err := tx.Exec(`INSERT OR REPLACE INTO videos (user_id, id, created_at, title, url, thumbnail_url, views, duration_s)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, v.ID, parseHelixTime(v.CreatedAt).Unix(), v.Title, v.URL, v.ThumbnailURL,
				v.ViewCount, int64(d.Seconds())); err != nil {
				return err
			}
		}
	}
	if clips != nil {
		if _, err := tx.Exec(`DELETE FROM clips WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, c := range clips {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO clips (user_id, id, created_at, title, url, thumbnail_url, views, duration_s, creator_name)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, userID, c.ID, parseHelixTime(c.CreatedAt).Unix(), c.Title, c.URL, c.ThumbnailURL,
				c.ViewCount, c.Duration, c.CreatorName); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO media_fetched (user_id, day) VALUES (?, ?)`, userID, now.UTC().Format(time.DateOnly)); err != nil {
		return err
	}
	return tx.Commit()
}

// fetchMedia refreshes VODs (last mediaCount archives) and clips (top
// mediaCount by views from the last 30 days) for up to mediaBatch roster
// streamers not yet done today.
func (s *StreamersRepo) fetchMedia() {
	l := s.live
	if l == nil || s.media == nil || l.roster == nil {
		return
	}
	now := timeNow()
	done := map[string]bool{}
	rows, err := l.db.db.Query(`SELECT user_id FROM media_fetched WHERE day = ?`, now.UTC().Format(time.DateOnly))
	if err != nil {
		log.Errorf("stats: reading media fetches: %s", err)
		return
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		done[id] = true
	}
	rows.Close()
	var todo []string
	for id := range l.roster {
		if !done[id] {
			todo = append(todo, id)
		}
	}
	sort.Strings(todo)
	if len(todo) > mediaBatch {
		todo = todo[:mediaBatch]
	}
	failed := 0
	for _, id := range todo {
		vr, err := s.media.GetVideos(&helix.VideosParams{UserID: id, Type: "archive", First: mediaCount})
		if err == nil {
			err = helixError(vr.ResponseCommon)
		}
		if err != nil {
			failed++
			log.Warnf("stats: VODs of %s: %s", id, err)
			if failed >= 3 {
				return
			}
			continue
		}
		cr, err := s.media.GetClips(&helix.ClipsParams{BroadcasterID: id, First: mediaCount,
			StartedAt: helix.Time{Time: now.Add(-30 * day).UTC().Truncate(time.Second)}})
		if err == nil {
			err = helixError(cr.ResponseCommon)
		}
		if err != nil {
			failed++
			log.Warnf("stats: clips of %s: %s", id, err)
			if failed >= 3 {
				return
			}
			continue
		}
		videos, clips := vr.Data.Videos, cr.Data.Clips
		if videos == nil {
			videos = []helix.Video{}
		}
		if clips == nil {
			clips = []helix.Clip{}
		}
		if err := l.db.replaceMedia(id, videos, clips, now); err != nil {
			log.Errorf("stats: saving media of %s: %s", id, err)
			return
		}
	}
}

func (st *store) loadMedia(table string) (map[string][]mediaItem, error) {
	cols := `id, created_at, title, url, thumbnail_url, views, duration_s, ''`
	if table == "clips" {
		cols = `id, created_at, title, url, thumbnail_url, views, duration_s, creator_name`
	}
	rows, err := st.db.Query(`SELECT user_id, ` + cols + ` FROM ` + table + ` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]mediaItem{}
	for rows.Next() {
		var id string
		var created int64
		var m mediaItem
		if err := rows.Scan(&id, &m.ID, &created, &m.Title, &m.URL, &m.ThumbnailURL, &m.Views, &m.DurationS, &m.CreatorName); err != nil {
			return nil, err
		}
		m.CreatedAt = rfc3339(time.Unix(created, 0))
		out[id] = append(out[id], m)
	}
	return out, rows.Err()
}
