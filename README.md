# movie-night

A website to track what to watch for movie night. Everyone suggests movies and
votes on what to watch next — one vote per person.

Built with Go + Postgres on the backend and [htmx](https://htmx.org) on the front end.

## Running

```sh
cp .env.example .env   # add your OMDb API key (https://www.omdbapi.com/apikey.aspx)
docker compose up --build
```

Then open http://localhost:8080, create an account, and start adding movies.
Typing in the suggest box searches [OMDb](https://www.omdbapi.com) by title so
you can pick the exact movie (with year and poster). Search results are cached
in Postgres forever, so a repeat search never spends API credits. Without an
API key the app still works — you just add titles as typed.

## How it works

- **Auth** — simple username/password accounts (bcrypt-hashed) with cookie
  sessions stored in Postgres, so every voter is unique.
- **One vote per person** — enforced by the database: `votes.user_id` is the
  primary key. Clicking *Vote* on another movie moves your vote; clicking your
  current vote removes it.
- **Watched** — marking a movie as watched moves it to the history and clears
  all votes for the next round.

## Development

Run Postgres via compose and the app locally:

```sh
docker compose up -d db
DATABASE_URL="postgres://movienight:movienight@localhost:5432/movienight?sslmode=disable" go run .
```

The schema is applied automatically on startup.
