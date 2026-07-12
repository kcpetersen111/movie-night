package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

type App struct {
	store *Store
	tmpl  *template.Template
	omdb  *OMDB
}

type ctxKey string

const userKey ctxKey = "user"
const theaterKey ctxKey = "theater"

const sessionCookie = "movie_night_session"

type loginData struct {
	Error string
}

type theatersData struct {
	Theaters []Theater
	Error    string
}

type pageData struct {
	LoggedIn    bool
	Username    string
	TheaterID   int
	TheaterName string
	Movies      []MovieRow
	Watched     []WatchedRow
}

// --- auth ---

func (a *App) requireUser(next func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err == nil {
			user, err := a.store.GetSessionUser(r.Context(), cookie.Value)
			if err == nil {
				next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
				return
			}
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// requireTheaterMember reads {theaterID} from the path and confirms the
// current user belongs to it, stashing the Theater in context. Must be
// wrapped by requireUser so currentUser(r) is already populated.
func (a *App) requireTheaterMember(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		theaterID, err := strconv.Atoi(r.PathValue("theaterID"))
		if err != nil {
			http.Error(w, "bad theater id", http.StatusBadRequest)
			return
		}
		ok, err := a.store.IsMember(r.Context(), theaterID, currentUser(r).ID)
		if err != nil {
			a.serverError(w, err)
			return
		}
		if !ok {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/theaters")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			http.Redirect(w, r, "/theaters", http.StatusSeeOther)
			return
		}
		theater, err := a.store.GetTheater(r.Context(), theaterID)
		if err != nil {
			a.serverError(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), theaterKey, theater)))
	}
}

func currentUser(r *http.Request) User {
	u, _ := r.Context().Value(userKey).(User)
	return u
}

func currentTheater(r *http.Request) Theater {
	t, _ := r.Context().Value(theaterKey).(Theater)
	return t
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if _, err := a.store.GetSessionUser(r.Context(), cookie.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	a.render(w, "login.html", loginData{})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	id, hash, err := a.store.GetUserCredentials(r.Context(), username)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		w.WriteHeader(http.StatusUnauthorized)
		a.render(w, "login.html", loginData{Error: "Invalid username or password."})
		return
	}
	a.startSession(w, r, id)
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if username == "" || len(username) > 50 {
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, "login.html", loginData{Error: "Username must be 1-50 characters."})
		return
	}
	if len(password) < 4 {
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, "login.html", loginData{Error: "Password must be at least 4 characters."})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		a.serverError(w, err)
		return
	}
	id, err := a.store.CreateUser(r.Context(), username, string(hash))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			w.WriteHeader(http.StatusConflict)
			a.render(w, "login.html", loginData{Error: "That username is already taken."})
			return
		}
		a.serverError(w, err)
		return
	}
	a.startSession(w, r, id)
}

func (a *App) startSession(w http.ResponseWriter, r *http.Request, userID int) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		a.serverError(w, err)
		return
	}
	token := hex.EncodeToString(buf)
	if err := a.store.CreateSession(r.Context(), token, userID); err != nil {
		a.serverError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30 * 24 * 60 * 60,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = a.store.DeleteSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- pages ---

// rootRedirect sends a logged-in user straight to their theater if they
// belong to exactly one, otherwise to the theater picker.
func (a *App) rootRedirect(w http.ResponseWriter, r *http.Request) {
	theaters, err := a.store.ListUserTheaters(r.Context(), currentUser(r).ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if len(theaters) == 1 {
		http.Redirect(w, r, fmt.Sprintf("/t/%d/", theaters[0].ID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/theaters", http.StatusSeeOther)
}

func (a *App) theatersPage(w http.ResponseWriter, r *http.Request) {
	theaters, err := a.store.ListUserTheaters(r.Context(), currentUser(r).ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, "theaters.html", theatersData{Theaters: theaters})
}

func (a *App) createTheater(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 100 {
		theaters, _ := a.store.ListUserTheaters(r.Context(), currentUser(r).ID)
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, "theaters.html", theatersData{Theaters: theaters, Error: "Theater name must be 1-100 characters."})
		return
	}
	theater, err := a.store.CreateTheater(r.Context(), name, currentUser(r).ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/t/%d/", theater.ID), http.StatusSeeOther)
}

func (a *App) joinTheater(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(strings.ToLower(r.FormValue("code")))
	theater, err := a.store.JoinTheaterByCode(r.Context(), code, currentUser(r).ID)
	if err != nil {
		theaters, _ := a.store.ListUserTheaters(r.Context(), currentUser(r).ID)
		w.WriteHeader(http.StatusNotFound)
		a.render(w, "theaters.html", theatersData{Theaters: theaters, Error: "That invite code doesn't match any theater."})
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/t/%d/", theater.ID), http.StatusSeeOther)
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	data, err := a.boardData(r)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, "index.html", data)
}

func (a *App) addMovie(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.FormValue("title"))
	year := strings.TrimSpace(r.FormValue("year"))
	poster := strings.TrimSpace(r.FormValue("poster"))
	if title != "" && len(title) <= 200 && len(year) <= 20 && len(poster) <= 500 {
		if err := a.store.AddMovie(r.Context(), currentTheater(r).ID, title, year, poster, currentUser(r).ID); err != nil {
			a.serverError(w, err)
			return
		}
	}
	a.renderBoard(w, r)
}

type searchData struct {
	Results []SearchResult
	Message string
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.FormValue("title"))
	if len(query) < 2 {
		a.render(w, "search-results", searchData{})
		return
	}

	// Cache key: case- and whitespace-insensitive so "The Matrix" and
	// "the  matrix" share one entry.
	key := strings.ToLower(strings.Join(strings.Fields(query), " "))

	results, hit, err := a.store.GetCachedSearch(r.Context(), key)
	if err != nil {
		log.Printf("search cache read %q: %v", key, err)
	}
	if !hit {
		if a.omdb == nil {
			a.render(w, "search-results", searchData{
				Message: "Movie search is not configured — set OMDB_API_KEY in .env. You can still add the title as typed.",
			})
			return
		}
		results, err = a.omdb.Search(r.Context(), query)
		if err != nil {
			log.Printf("omdb search %q: %v", query, err)
			a.render(w, "search-results", searchData{Message: "Search failed — you can still add the title as typed."})
			return
		}
		if err := a.store.CacheSearch(r.Context(), key, results); err != nil {
			log.Printf("search cache write %q: %v", key, err)
		}
	}

	if len(results) > 6 {
		results = results[:6]
	}
	a.render(w, "search-results", searchData{Results: results})
}

func (a *App) vote(w http.ResponseWriter, r *http.Request) {
	movieID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad movie id", http.StatusBadRequest)
		return
	}
	if err := a.store.ToggleVote(r.Context(), currentTheater(r).ID, currentUser(r).ID, movieID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.serverError(w, err)
		return
	}
	a.renderBoard(w, r)
}

func (a *App) markWatched(w http.ResponseWriter, r *http.Request) {
	movieID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad movie id", http.StatusBadRequest)
		return
	}
	if err := a.store.MarkWatched(r.Context(), currentTheater(r).ID, movieID); err != nil {
		a.serverError(w, err)
		return
	}
	a.renderBoard(w, r)
}

// --- helpers ---

func (a *App) boardData(r *http.Request) (pageData, error) {
	user := currentUser(r)
	theater := currentTheater(r)
	movies, err := a.store.ListMovies(r.Context(), theater.ID, user.ID)
	if err != nil {
		return pageData{}, err
	}
	watched, err := a.store.ListWatched(r.Context(), theater.ID)
	if err != nil {
		return pageData{}, err
	}
	return pageData{
		LoggedIn:    user.ID != 0,
		Username:    user.Username,
		TheaterID:   theater.ID,
		TheaterName: theater.Name,
		Movies:      movies,
		Watched:     watched,
	}, nil
}

func (a *App) renderBoard(w http.ResponseWriter, r *http.Request) {
	data, err := a.boardData(r)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, "movie-board", data)
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	log.Printf("server error: %v", err)
	http.Error(w, "something went wrong", http.StatusInternalServerError)
}
