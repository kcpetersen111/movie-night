package main

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

func main() {
	loadDotEnv(".env")

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://movienight:movienight@localhost:5432/movienight?sslmode=disable"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3411"
	}

	tmpl := template.Must(template.ParseFS(templateFS, "templates/*.html"))

	pool := mustConnect(dbURL)
	defer pool.Close()

	omdb := NewOMDB(os.Getenv("OMDB_API_KEY"))
	if omdb == nil {
		log.Print("OMDB_API_KEY not set — movie search disabled, manual titles still work")
	}

	app := &App{store: &Store{pool: pool}, tmpl: tmpl, omdb: omdb, hub: NewHub()}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatal(err)
	}
	staticServer := http.FileServerFS(static)

	mux := http.NewServeMux()
	for _, path := range []string{
		"/favicon.ico", "/favicon-16x16.png", "/favicon-32x32.png",
		"/apple-touch-icon.png", "/android-chrome-192x192.png", "/android-chrome-512x512.png",
		"/site.webmanifest",
	} {
		mux.Handle("GET "+path, staticServer)
	}
	mux.HandleFunc("GET /login", app.loginPage)
	mux.HandleFunc("POST /login", app.login)
	mux.HandleFunc("POST /register", app.register)
	mux.HandleFunc("POST /logout", app.logout)
	mux.HandleFunc("GET /invite/{code}", app.invite)
	mux.Handle("GET /{$}", app.requireUser(app.rootRedirect))
	mux.Handle("GET /theaters", app.requireUser(app.theatersPage))
	mux.Handle("POST /theaters", app.requireUser(app.createTheater))
	mux.Handle("POST /theaters/join", app.requireUser(app.joinTheater))
	mux.Handle("GET /ws", app.requireUser(app.serveWS))
	mux.Handle("GET /t/{theaterID}/", app.requireUser(app.requireTheaterMember(app.index)))
	mux.Handle("GET /t/{theaterID}/search", app.requireUser(app.requireTheaterMember(app.search)))
	mux.Handle("GET /t/{theaterID}/board", app.requireUser(app.requireTheaterMember(app.board)))
	mux.Handle("GET /t/{theaterID}/overview", app.requireUser(app.requireTheaterMember(app.theaterOverview)))
	mux.Handle("POST /t/{theaterID}/delete", app.requireUser(app.requireTheaterMember(app.deleteTheater)))
	mux.Handle("POST /t/{theaterID}/members/{userID}/remove", app.requireUser(app.requireTheaterMember(app.removeMember)))
	mux.Handle("POST /t/{theaterID}/movies", app.requireUser(app.requireTheaterMember(app.addMovie)))
	mux.Handle("POST /t/{theaterID}/vote/{id}", app.requireUser(app.requireTheaterMember(app.vote)))
	mux.Handle("POST /t/{theaterID}/watched/{id}", app.requireUser(app.requireTheaterMember(app.markWatched)))

	log.Printf("movie-night listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// loadDotEnv reads KEY=VALUE lines from a .env file into the environment.
// Real environment variables take precedence; a missing file is fine.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
}

// mustConnect retries so the app can come up alongside postgres in compose.
func mustConnect(url string) *pgxpool.Pool {
	var pool *pgxpool.Pool
	var err error
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		pool, err = pgxpool.New(ctx, url)
		if err == nil {
			err = pool.Ping(ctx)
		}
		if err == nil {
			err = migrate(ctx, pool)
		}
		cancel()
		if err == nil {
			return pool
		}
		if pool != nil {
			pool.Close()
		}
		log.Printf("waiting for database: %v", err)
		time.Sleep(time.Second)
	}
	log.Fatalf("could not connect to database: %v", err)
	return nil
}
