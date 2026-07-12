package main

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/*.html
var templateFS embed.FS

func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://movienight:movienight@localhost:5432/movienight?sslmode=disable"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	tmpl := template.Must(template.ParseFS(templateFS, "templates/*.html"))

	pool := mustConnect(dbURL)
	defer pool.Close()

	app := &App{store: &Store{pool: pool}, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", app.loginPage)
	mux.HandleFunc("POST /login", app.login)
	mux.HandleFunc("POST /register", app.register)
	mux.HandleFunc("POST /logout", app.logout)
	mux.Handle("GET /{$}", app.requireUser(app.index))
	mux.Handle("POST /movies", app.requireUser(app.addMovie))
	mux.Handle("POST /vote/{id}", app.requireUser(app.vote))
	mux.Handle("POST /watched/{id}", app.requireUser(app.markWatched))

	log.Printf("movie-night listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
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
