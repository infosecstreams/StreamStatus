package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// defaultDBPath is where the stream history lives (SS_DB_PATH). The host
// mounts a volume at /data.
const defaultDBPath = "/data/streamstatus.db"

// store is the bot's SQLite database: the Twitch user ID <-> login map and
// the recorded stream sessions. Only the worker goroutine uses it.
//
// A session is one broadcast (stream.online .. stream.offline), identified
// by the broadcaster ID and Twitch's started_at. It is split into segments
// at every category change, so time can be counted per category.
type store struct {
	db *sql.DB
}

// migrations are applied in order; PRAGMA user_version records how many
// have run. Never edit a released migration, append a new one.
var migrations = []string{
	`CREATE TABLE users (
		id           TEXT PRIMARY KEY,
		login        TEXT NOT NULL,
		display_name TEXT NOT NULL DEFAULT '',
		updated_at   INTEGER NOT NULL
	);
	CREATE TABLE user_logins (
		login      TEXT NOT NULL,
		user_id    TEXT NOT NULL,
		first_seen INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL,
		PRIMARY KEY (login, user_id)
	);
	CREATE TABLE sessions (
		id         INTEGER PRIMARY KEY,
		user_id    TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		ended_at   INTEGER,
		last_seen  INTEGER NOT NULL,
		UNIQUE (user_id, started_at)
	);
	CREATE INDEX sessions_open ON sessions (user_id) WHERE ended_at IS NULL;
	CREATE TABLE segments (
		id         INTEGER PRIMARY KEY,
		session_id INTEGER NOT NULL REFERENCES sessions (id),
		user_id    TEXT NOT NULL,
		category   TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		ended_at   INTEGER
	);
	CREATE INDEX segments_session ON segments (session_id);
	CREATE INDEX segments_ended ON segments (ended_at);`,

	// 2: statistics. Viewer counts sampled while live, a daily follower
	// total, title changes, and when recording started.
	`CREATE TABLE viewer_samples (
		user_id  TEXT NOT NULL,
		at       INTEGER NOT NULL,
		viewers  INTEGER NOT NULL,
		category TEXT NOT NULL,
		PRIMARY KEY (user_id, at)
	);
	CREATE INDEX viewer_samples_at ON viewer_samples (at);
	CREATE TABLE follower_counts (
		user_id TEXT NOT NULL,
		day     TEXT NOT NULL,
		at      INTEGER NOT NULL,
		total   INTEGER NOT NULL,
		PRIMARY KEY (user_id, day)
	);
	CREATE TABLE titles (
		id       INTEGER PRIMARY KEY,
		user_id  TEXT NOT NULL,
		at       INTEGER NOT NULL,
		title    TEXT NOT NULL,
		category TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX titles_user ON titles (user_id, at);
	CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	INSERT INTO meta (key, value)
		SELECT 'recording_since', CAST(COALESCE(MIN(started_at), CAST(strftime('%s', 'now') AS INTEGER)) AS TEXT) FROM sessions;`,

	// 3: where a session came from: "live" (recorded by the bot) or
	// "backfill" (imported from a tracker's history, see backfill.go).
	`ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT 'live';`,
}

// openStore opens (creating if needed) the database at path and brings
// its schema up to date.
func openStore(path string) (*store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: the worker is the only user, and it keeps
	// transactions and pragmas on the same connection.
	db.SetMaxOpenConns(1)
	st := &store{db: db}
	if err := st.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return st, nil
}

func (st *store) close() error { return st.db.Close() }

func (st *store) migrate() error {
	var version int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := st.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// userRow is one Twitch account.
type userRow struct {
	id, login, displayName string
}

// upsertUser records id's current login (and display name, if given).
// Earlier logins stay in user_logins, so a roster entry that still uses
// an old login keeps mapping to the same ID.
func (st *store) upsertUser(id, login, displayName string, at time.Time) error {
	login = strings.ToLower(strings.TrimSpace(login))
	if id == "" || login == "" {
		return nil
	}
	now := at.Unix()
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO users (id, login, display_name, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET login = excluded.login,
			display_name = CASE WHEN excluded.display_name = '' THEN users.display_name ELSE excluded.display_name END,
			updated_at = excluded.updated_at`, id, login, displayName, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO user_logins (login, user_id, first_seen, last_seen) VALUES (?, ?, ?, ?)
		ON CONFLICT (login, user_id) DO UPDATE SET last_seen = excluded.last_seen`, login, id, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// loadUsers returns every known user and a map of every login ever seen
// (lowercased) to its user ID. When a login was used by more than one
// account, the account that used it most recently wins, except that a
// user's current login always maps to that user.
func (st *store) loadUsers() ([]userRow, map[string]string, error) {
	rows, err := st.db.Query(`SELECT id, login, display_name FROM users`)
	if err != nil {
		return nil, nil, err
	}
	var users []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.id, &u.login, &u.displayName); err != nil {
			rows.Close()
			return nil, nil, err
		}
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	byLogin := map[string]string{}
	rows, err = st.db.Query(`SELECT login, user_id FROM user_logins ORDER BY last_seen`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var login, id string
		if err := rows.Scan(&login, &id); err != nil {
			rows.Close()
			return nil, nil, err
		}
		byLogin[login] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, u := range users {
		byLogin[u.login] = u.id
	}
	return users, byLogin, nil
}

// openSession is the open session of a user, if any.
type openSession struct {
	id        int64
	startedAt time.Time
	lastSeen  time.Time
	category  string // category of the open segment
}

func (st *store) openSessionTx(tx *sql.Tx, userID string) (*openSession, error) {
	var o openSession
	var started, seen int64
	err := tx.QueryRow(`SELECT s.id, s.started_at, s.last_seen, COALESCE(g.category, '')
		FROM sessions s LEFT JOIN segments g ON g.session_id = s.id AND g.ended_at IS NULL
		WHERE s.user_id = ? AND s.ended_at IS NULL
		ORDER BY s.started_at DESC LIMIT 1`, userID).Scan(&o.id, &started, &seen, &o.category)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.startedAt, o.lastSeen = time.Unix(started, 0).UTC(), time.Unix(seen, 0).UTC()
	return &o, nil
}

// closeSessionTx ends session id (and its open segment) at end, never
// before the segment or session started.
func closeSessionTx(tx *sql.Tx, id int64, end time.Time) error {
	e := end.Unix()
	if _, err := tx.Exec(`UPDATE segments SET ended_at = MAX(started_at, ?) WHERE session_id = ? AND ended_at IS NULL`, e, id); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE sessions SET ended_at = MAX(started_at, ?), last_seen = MAX(last_seen, MAX(started_at, ?)) WHERE id = ?`, e, e, id)
	return err
}

// streamSeen records that userID is live in category, in the broadcast
// Twitch says started at startedAt, as observed at now. It opens a session
// if none is open, continues the open one if it is the same broadcast, and
// splits it if the category changed. If an open session belongs to an
// earlier broadcast (its stream.offline was missed), that one is closed at
// the last time it was seen live first.
//
// stale is true if this broadcast was already recorded as ended: Helix
// still lists a stream for a minute or two after stream.offline, and such
// an observation must not reopen it.
func (st *store) streamSeen(userID string, startedAt time.Time, category string, now time.Time) (stale bool, err error) {
	if startedAt.IsZero() || startedAt.After(now) {
		startedAt = now
	}
	tx, err := st.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	open, err := st.openSessionTx(tx, userID)
	if err != nil {
		return false, err
	}
	if open != nil && open.startedAt.Equal(startedAt.Truncate(time.Second)) {
		if _, err := tx.Exec(`UPDATE sessions SET last_seen = MAX(last_seen, ?) WHERE id = ?`, now.Unix(), open.id); err != nil {
			return false, err
		}
		if open.category != category {
			if err := splitTx(tx, open.id, userID, category, now); err != nil {
				return false, err
			}
		}
		return false, tx.Commit()
	}
	if open != nil && startedAt.Before(open.startedAt) {
		// An older broadcast than the one in progress: stale Helix data.
		return true, tx.Commit()
	}
	if open != nil {
		if err := closeSessionTx(tx, open.id, open.lastSeen); err != nil {
			return false, err
		}
	}

	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND started_at = ?`, userID, startedAt.Unix()).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, tx.Commit()
	}
	res, err := tx.Exec(`INSERT INTO sessions (user_id, started_at, last_seen) VALUES (?, ?, ?)`, userID, startedAt.Unix(), now.Unix())
	if err != nil {
		return false, err
	}
	sid, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO segments (session_id, user_id, category, started_at) VALUES (?, ?, ?, ?)`, sid, userID, category, startedAt.Unix()); err != nil {
		return false, err
	}
	return false, tx.Commit()
}

// splitTx ends session sid's open segment at at and starts one in category.
func splitTx(tx *sql.Tx, sid int64, userID, category string, at time.Time) error {
	if _, err := tx.Exec(`UPDATE segments SET ended_at = MAX(started_at, ?) WHERE session_id = ? AND ended_at IS NULL`, at.Unix(), sid); err != nil {
		return err
	}
	var start int64
	// The new segment can't start before the previous one ended.
	if err := tx.QueryRow(`SELECT MAX(?, COALESCE(MAX(ended_at), 0)) FROM segments WHERE session_id = ?`, at.Unix(), sid).Scan(&start); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO segments (session_id, user_id, category, started_at) VALUES (?, ?, ?, ?)`, sid, userID, category, start)
	return err
}

// categoryChanged splits userID's open session at at if its category
// differs. It does nothing when no session is open.
func (st *store) categoryChanged(userID, category string, at time.Time) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	open, err := st.openSessionTx(tx, userID)
	if err != nil || open == nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sessions SET last_seen = MAX(last_seen, ?) WHERE id = ?`, at.Unix(), open.id); err != nil {
		return err
	}
	if open.category != category {
		if err := splitTx(tx, open.id, userID, category, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// streamEnded closes userID's open session at at (stream.offline).
func (st *store) streamEnded(userID string, at time.Time) error {
	return st.endSession(userID, func(*openSession) time.Time { return at })
}

// streamMissing closes userID's open session at the last time it was seen
// live: reconciliation found them offline, so the offline event was missed
// (or the bot was down when it came) and the real end is unknown.
func (st *store) streamMissing(userID string) error {
	return st.endSession(userID, func(o *openSession) time.Time { return o.lastSeen })
}

func (st *store) endSession(userID string, end func(*openSession) time.Time) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	open, err := st.openSessionTx(tx, userID)
	if err != nil || open == nil {
		return err
	}
	if err := closeSessionTx(tx, open.id, end(open)); err != nil {
		return err
	}
	return tx.Commit()
}

// hasOpenSession reports whether userID has a session in progress.
func (st *store) hasOpenSession(userID string) (bool, error) {
	var n int
	err := st.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND ended_at IS NULL`, userID).Scan(&n)
	return n > 0, err
}

// openSessionUsers returns the IDs of users with a session in progress.
func (st *store) openSessionUsers() ([]string, error) {
	rows, err := st.db.Query(`SELECT DISTINCT user_id FROM sessions WHERE ended_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// segment is a stretch of a session in one category. end is zero while it
// is still open.
type segment struct {
	userID     string
	category   string
	start, end time.Time
}

// segmentsSince returns every segment that was still running at or after
// from.
func (st *store) segmentsSince(from time.Time) ([]segment, error) {
	rows, err := st.db.Query(`SELECT user_id, category, started_at, ended_at FROM segments
		WHERE ended_at IS NULL OR ended_at > ?`, from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []segment
	for rows.Next() {
		var g segment
		var start int64
		var end sql.NullInt64
		if err := rows.Scan(&g.userID, &g.category, &start, &end); err != nil {
			return nil, err
		}
		g.start = time.Unix(start, 0).UTC()
		if end.Valid {
			g.end = time.Unix(end.Int64, 0).UTC()
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// hoursWindow is the period hours_30d covers.
const hoursWindow = 30 * 24 * time.Hour

// streamedHours sums, per user, the time in segs that falls inside
// (now-window, now] and was spent in one of VALID_GAMES. Open segments run
// until now. This mirrors SullyGnome's "hours streamed in the last 30
// days", restricted to the infosec categories.
func streamedHours(segs []segment, now time.Time, window time.Duration) map[string]float64 {
	from := now.Add(-window)
	secs := map[string]float64{}
	for _, g := range segs {
		if !countsAsInfosec(g.category) {
			continue
		}
		start, end := g.start, g.end
		if end.IsZero() || end.After(now) {
			end = now
		}
		if start.Before(from) {
			start = from
		}
		if !end.After(start) {
			continue
		}
		secs[g.userID] += end.Sub(start).Seconds()
	}
	hours := make(map[string]float64, len(secs))
	for id, s := range secs {
		hours[id] = s / 3600
	}
	return hours
}
