package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id SERIAL PRIMARY KEY,
	username TEXT UNIQUE NOT NULL,
	password_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS movies (
	id SERIAL PRIMARY KEY,
	title TEXT NOT NULL,
	added_by INT NOT NULL REFERENCES users(id),
	watched BOOLEAN NOT NULL DEFAULT false,
	watched_at TIMESTAMPTZ,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- user_id is the primary key: each user has at most one active vote.
CREATE TABLE IF NOT EXISTS votes (
	user_id INT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	movie_id INT NOT NULL REFERENCES movies(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
	token TEXT PRIMARY KEY,
	user_id INT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schema)
	return err
}

type Store struct {
	pool *pgxpool.Pool
}

type User struct {
	ID       int
	Username string
}

type MovieRow struct {
	ID        int
	Title     string
	AddedBy   string
	Votes     int
	VotedByMe bool
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

func (s *Store) AddMovie(ctx context.Context, title string, userID int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO movies (title, added_by) VALUES ($1, $2)`, title, userID)
	return err
}

func (s *Store) ListMovies(ctx context.Context, currentUserID int) ([]MovieRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.title, u.username,
		        count(v.user_id) AS votes,
		        coalesce(bool_or(v.user_id = $1), false) AS voted_by_me
		 FROM movies m
		 JOIN users u ON u.id = m.added_by
		 LEFT JOIN votes v ON v.movie_id = m.id
		 WHERE NOT m.watched
		 GROUP BY m.id, m.title, u.username
		 ORDER BY votes DESC, m.created_at ASC`, currentUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var movies []MovieRow
	for rows.Next() {
		var m MovieRow
		if err := rows.Scan(&m.ID, &m.Title, &m.AddedBy, &m.Votes, &m.VotedByMe); err != nil {
			return nil, err
		}
		movies = append(movies, m)
	}
	return movies, rows.Err()
}

func (s *Store) ListWatched(ctx context.Context) ([]WatchedRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT title, watched_at FROM movies WHERE watched ORDER BY watched_at DESC LIMIT 10`)
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

// ToggleVote removes the user's vote if it is already on this movie,
// otherwise moves their single vote to it. The primary key on votes.user_id
// guarantees one vote per user no matter what.
func (s *Store) ToggleVote(ctx context.Context, userID, movieID int) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`DELETE FROM votes WHERE user_id = $1 AND movie_id = $2`, userID, movieID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		_, err = tx.Exec(ctx,
			`INSERT INTO votes (user_id, movie_id)
			 SELECT $1, $2 WHERE EXISTS (SELECT 1 FROM movies WHERE id = $2 AND NOT watched)
			 ON CONFLICT (user_id) DO UPDATE SET movie_id = EXCLUDED.movie_id, created_at = now()`,
			userID, movieID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// MarkWatched flags the movie and clears all votes to start the next round.
func (s *Store) MarkWatched(ctx context.Context, movieID int) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE movies SET watched = true, watched_at = now() WHERE id = $1 AND NOT watched`,
		movieID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM votes`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
