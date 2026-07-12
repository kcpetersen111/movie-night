package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id SERIAL PRIMARY KEY,
	username TEXT UNIQUE NOT NULL,
	password_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS theaters (
	id SERIAL PRIMARY KEY,
	name TEXT NOT NULL,
	invite_code TEXT UNIQUE NOT NULL,
	created_by INT NOT NULL REFERENCES users(id),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS theater_members (
	theater_id INT NOT NULL REFERENCES theaters(id) ON DELETE CASCADE,
	user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (theater_id, user_id)
);

CREATE TABLE IF NOT EXISTS movies (
	id SERIAL PRIMARY KEY,
	title TEXT NOT NULL,
	year TEXT NOT NULL DEFAULT '',
	poster TEXT NOT NULL DEFAULT '',
	added_by INT NOT NULL REFERENCES users(id),
	watched BOOLEAN NOT NULL DEFAULT false,
	watched_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE movies ADD COLUMN IF NOT EXISTS year TEXT NOT NULL DEFAULT '';
ALTER TABLE movies ADD COLUMN IF NOT EXISTS poster TEXT NOT NULL DEFAULT '';
ALTER TABLE movies ADD COLUMN IF NOT EXISTS theater_id INT REFERENCES theaters(id) ON DELETE CASCADE;

-- user_id is part of the primary key: each user has at most one active
-- vote per theater (see backfillTheaters, which upgrades the PK to
-- (user_id, theater_id) once theater_id is backfilled).
CREATE TABLE IF NOT EXISTS votes (
	user_id INT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	movie_id INT NOT NULL REFERENCES movies(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE votes ADD COLUMN IF NOT EXISTS theater_id INT REFERENCES theaters(id) ON DELETE CASCADE;

CREATE TABLE IF NOT EXISTS search_cache (
	query TEXT PRIMARY KEY,
	results JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
	token TEXT PRIMARY KEY,
	user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schema); err != nil {
		return err
	}
	return backfillTheaters(ctx, pool)
}

// backfillTheaters is a one-time upgrade step for installs that predate
// theaters: it creates a "General" theater, makes every existing user a
// member, assigns all existing movies/votes to it, and tightens
// theater_id to NOT NULL (including widening the votes primary key to
// (user_id, theater_id)). It no-ops on every subsequent boot once
// votes.theater_id is already NOT NULL.
func backfillTheaters(ctx context.Context, pool *pgxpool.Pool) error {
	var alreadyMigrated bool
	err := pool.QueryRow(ctx,
		`SELECT is_nullable = 'NO' FROM information_schema.columns
		 WHERE table_name = 'votes' AND column_name = 'theater_id'`).Scan(&alreadyMigrated)
	if err != nil {
		return err
	}
	if alreadyMigrated {
		return nil
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	code, err := randomCode()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO theaters (name, invite_code, created_by)
		 SELECT 'General', $1, (SELECT id FROM users ORDER BY id LIMIT 1)
		 WHERE NOT EXISTS (SELECT 1 FROM theaters) AND EXISTS (SELECT 1 FROM users)`,
		code); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO theater_members (theater_id, user_id)
		 SELECT (SELECT id FROM theaters ORDER BY id LIMIT 1), id FROM users
		 ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE movies SET theater_id = (SELECT id FROM theaters ORDER BY id LIMIT 1) WHERE theater_id IS NULL`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE votes SET theater_id = (SELECT id FROM theaters ORDER BY id LIMIT 1) WHERE theater_id IS NULL`); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `ALTER TABLE movies ALTER COLUMN theater_id SET NOT NULL`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE votes ALTER COLUMN theater_id SET NOT NULL`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE votes DROP CONSTRAINT votes_pkey`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE votes ADD CONSTRAINT votes_pkey PRIMARY KEY (user_id, theater_id)`); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// randomCode returns a short random invite code shared by theaters'
// invite_code and used as a fallback default when none exists yet.
func randomCode() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

type Store struct {
	pool *pgxpool.Pool
}

type User struct {
	ID       int
	Username string
}

type Theater struct {
	ID         int
	Name       string
	InviteCode string
	CreatedBy  int
}

type MovieRow struct {
	ID        int
	Title     string
	Year      string
	Poster    string
	AddedBy   string
	Votes     int
	VotedByMe bool
	AddedAt   time.Time
}

type WatchedRow struct {
	Title     string
	WatchedAt time.Time
}

func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (int, error) {
	var id int
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id`,
		username, passwordHash).Scan(&id)
	return id, err
}

func (s *Store) GetUserCredentials(ctx context.Context, username string) (id int, hash string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT id, password_hash FROM users WHERE username = $1`, username).Scan(&id, &hash)
	return id, hash, err
}

func (s *Store) CreateSession(ctx context.Context, token string, userID int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token, user_id) VALUES ($1, $2)`, token, userID)
	return err
}

func (s *Store) GetSessionUser(ctx context.Context, token string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT u.id, u.username FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 WHERE s.token = $1 AND s.created_at > now() - interval '30 days'`,
		token).Scan(&u.ID, &u.Username)
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token = $1`, token)
	return err
}

func (s *Store) AddMovie(ctx context.Context, theaterID int, title, year, poster string, userID int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO movies (theater_id, title, year, poster, added_by) VALUES ($1, $2, $3, $4, $5)`,
		theaterID, title, year, poster, userID)
	return err
}

func (s *Store) ListMovies(ctx context.Context, theaterID, currentUserID int) ([]MovieRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.title, m.year, m.poster, u.username,
		        count(v.user_id) AS votes,
		        coalesce(bool_or(v.user_id = $1), false) AS voted_by_me,
		        m.created_at
		 FROM movies m
		 JOIN users u ON u.id = m.added_by
		 LEFT JOIN votes v ON v.movie_id = m.id AND v.theater_id = m.theater_id
		 WHERE NOT m.watched AND m.theater_id = $2
		 GROUP BY m.id, u.username
		 ORDER BY votes DESC, m.created_at ASC`, currentUserID, theaterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var movies []MovieRow
	for rows.Next() {
		var m MovieRow
		if err := rows.Scan(&m.ID, &m.Title, &m.Year, &m.Poster, &m.AddedBy, &m.Votes, &m.VotedByMe, &m.AddedAt); err != nil {
			return nil, err
		}
		movies = append(movies, m)
	}
	return movies, rows.Err()
}

func (s *Store) ListWatched(ctx context.Context, theaterID int) ([]WatchedRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT title, watched_at FROM movies WHERE watched AND theater_id = $1 ORDER BY watched_at DESC LIMIT 10`,
		theaterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var watched []WatchedRow
	for rows.Next() {
		var w WatchedRow
		if err := rows.Scan(&w.Title, &w.WatchedAt); err != nil {
			return nil, err
		}
		watched = append(watched, w)
	}
	return watched, rows.Err()
}

func (s *Store) CreateTheater(ctx context.Context, name string, creatorID int) (Theater, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Theater{}, err
	}
	defer tx.Rollback(ctx)

	var t Theater
	t.Name = name
	t.CreatedBy = creatorID
	for attempt := 0; attempt < 5; attempt++ {
		code, err := randomCode()
		if err != nil {
			return Theater{}, err
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO theaters (name, invite_code, created_by) VALUES ($1, $2, $3) RETURNING id, invite_code`,
			name, code, creatorID).Scan(&t.ID, &t.InviteCode)
		if err == nil {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue // invite code collision, try again
		}
		return Theater{}, err
	}
	if t.ID == 0 {
		return Theater{}, errors.New("could not generate a unique invite code")
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO theater_members (theater_id, user_id) VALUES ($1, $2)`, t.ID, creatorID); err != nil {
		return Theater{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Theater{}, err
	}
	return t, nil
}

func (s *Store) JoinTheaterByCode(ctx context.Context, code string, userID int) (Theater, error) {
	var t Theater
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, invite_code FROM theaters WHERE invite_code = $1`, code).
		Scan(&t.ID, &t.Name, &t.InviteCode)
	if err != nil {
		return Theater{}, err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO theater_members (theater_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		t.ID, userID)
	if err != nil {
		return Theater{}, err
	}
	return t, nil
}

func (s *Store) ListUserTheaters(ctx context.Context, userID int) ([]Theater, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.id, t.name, t.invite_code FROM theaters t
		 JOIN theater_members tm ON tm.theater_id = t.id
		 WHERE tm.user_id = $1
		 ORDER BY t.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var theaters []Theater
	for rows.Next() {
		var t Theater
		if err := rows.Scan(&t.ID, &t.Name, &t.InviteCode); err != nil {
			return nil, err
		}
		theaters = append(theaters, t)
	}
	return theaters, rows.Err()
}

type MemberRow struct {
	UserID   int
	Username string
	JoinedAt time.Time
}

func (s *Store) ListMembers(ctx context.Context, theaterID int) ([]MemberRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT u.id, u.username, tm.joined_at FROM theater_members tm
		 JOIN users u ON u.id = tm.user_id
		 WHERE tm.theater_id = $1
		 ORDER BY tm.joined_at ASC`, theaterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []MemberRow
	for rows.Next() {
		var m MemberRow
		if err := rows.Scan(&m.UserID, &m.Username, &m.JoinedAt); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func (s *Store) IsMember(ctx context.Context, theaterID, userID int) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM theater_members WHERE theater_id = $1 AND user_id = $2)`,
		theaterID, userID).Scan(&ok)
	return ok, err
}

func (s *Store) GetTheater(ctx context.Context, theaterID int) (Theater, error) {
	var t Theater
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, invite_code, created_by FROM theaters WHERE id = $1`, theaterID).
		Scan(&t.ID, &t.Name, &t.InviteCode, &t.CreatedBy)
	return t, err
}

// DeleteTheater removes the theater and cascades to its members, movies,
// and votes.
func (s *Store) DeleteTheater(ctx context.Context, theaterID int) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM theaters WHERE id = $1`, theaterID)
	return err
}

// RemoveMember drops a user from a theater and clears their vote there, so
// they no longer count toward any pending movie's tally.
func (s *Store) RemoveMember(ctx context.Context, theaterID, userID int) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM votes WHERE theater_id = $1 AND user_id = $2`, theaterID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM theater_members WHERE theater_id = $1 AND user_id = $2`, theaterID, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetCachedSearch returns cached OMDb results for a normalized query.
// Empty result sets are cached too — "no matches" also costs an API credit.
func (s *Store) GetCachedSearch(ctx context.Context, query string) ([]SearchResult, bool, error) {
	var results []SearchResult
	err := s.pool.QueryRow(ctx,
		`SELECT results FROM search_cache WHERE query = $1`,
		query).Scan(&results)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return results, true, nil
}

func (s *Store) CacheSearch(ctx context.Context, query string, results []SearchResult) error {
	data, err := json.Marshal(results)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO search_cache (query, results) VALUES ($1, $2::jsonb)
		 ON CONFLICT (query) DO UPDATE SET results = EXCLUDED.results, created_at = now()`,
		query, string(data))
	return err
}

// ToggleVote removes the user's vote if it is already on this movie,
// otherwise moves their single vote (within this theater) to it. The
// primary key on votes (user_id, theater_id) guarantees one vote per user
// per theater no matter what.
func (s *Store) ToggleVote(ctx context.Context, theaterID, userID, movieID int) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`DELETE FROM votes WHERE user_id = $1 AND movie_id = $2 AND theater_id = $3`, userID, movieID, theaterID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, err = tx.Exec(ctx,
			`INSERT INTO votes (user_id, movie_id, theater_id)
			 SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM movies WHERE id = $2 AND theater_id = $3 AND NOT watched)
			 ON CONFLICT (user_id, theater_id) DO UPDATE SET movie_id = EXCLUDED.movie_id, created_at = now()`,
			userID, movieID, theaterID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// MarkWatched flags the movie and clears this theater's votes to start the
// next round.
func (s *Store) MarkWatched(ctx context.Context, theaterID, movieID int) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE movies SET watched = true, watched_at = now() WHERE id = $1 AND theater_id = $2 AND NOT watched`,
		movieID, theaterID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM votes WHERE theater_id = $1`, theaterID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
