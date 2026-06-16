# yt-transcript

> Extract **YouTube transcripts** from a single video, a list of URLs, or an
> entire channel — **no API key, no authentication**. A tiny, single-file Go
> service with a mobile-first web UI, a JSON/text REST API, and a
> CapRover-ready Dockerfile.

**Keywords:** youtube transcript · youtube captions · youtube subtitles ·
transcript downloader · channel transcripts · video transcription · Go · HTMX ·
Docker · CapRover · self-hosted.

A Go service (single file, standard library only) that extracts the
**transcript** (captions) of YouTube videos. It accepts any of:

- a **video URL** — `https://youtu.be/ID`, `.../watch?v=ID`, `.../shorts/ID`;
- a **list of URLs** — separated by spaces, commas or newlines;
- a **channel** — `.../@handle`, `.../channel/UC...`, `.../c/name`, `.../user/name`;
- a **playlist** — `.../playlist?list=...`.

For a channel or a playlist, the program fetches the list of videos (with
pagination) and then transcribes each of them.

## How it works

Transcripts are retrieved through YouTube's internal InnerTube API using the
`ANDROID` client, whose caption URLs are directly downloadable (no "Proof of
Origin Token" required). No API key and no authentication are needed.

## Running locally

```bash
go run . serve            # HTTP server on :8080 (or $PORT)
# or as a CLI:
go run . "https://www.youtube.com/@SamouraiDansant" --limit 5 --lang fr
go run . https://youtu.be/aircAruvnKk --json
```

CLI options: `--lang <code>`, `--limit <N>`, `--json` / `--text`.

## Web UI

A polished, mobile-first web UI (HTMX, no page reload) is served on `/`: paste a
video, a channel or a list of links, read the transcripts, and copy any of them
— or all at once — with a single tap. The page posts to `/ui/transcripts`, which
returns HTML fragments.

## HTTP API

`GET` or `POST` on `/api/transcripts`.

Parameters (query string or JSON body):

| parameter  | description                                              |
|------------|----------------------------------------------------------|
| `url`      | one input (repeatable) — video, channel or playlist      |
| `urls`     | multiple inputs separated by spaces/commas/newlines      |
| `channel`  | alias for `url`, for a channel                           |
| `lang`     | preferred transcript language (e.g. `fr`, `en`)          |
| `limit`    | max number of videos to transcribe (0 = unlimited)       |
| `segments` | `1` to include timestamped segments (JSON)               |
| `format`   | `json` (default) or `text`                               |

Examples:

```bash
# Plain text of a single video
curl "http://localhost:8080/api/transcripts?url=aircAruvnKk&lang=en&format=text"

# The 10 latest videos of a channel, as JSON
curl "http://localhost:8080/api/transcripts?channel=https://www.youtube.com/@SamouraiDansant&limit=10&lang=fr"

# Multiple URLs via POST JSON
curl -X POST http://localhost:8080/api/transcripts \
  -H 'Content-Type: application/json' \
  -d '{"urls":["https://youtu.be/aircAruvnKk","https://youtu.be/jNQXAC9IVRw"],"lang":"en","segments":true}'
```

## CapRover deployment

The repository ships a `Dockerfile` and a `captain-definition`.

1. Create an app in CapRover.
2. Under **App Configs → Container HTTP Port**, set **8080**.
3. Deploy (`caprover deploy`, Git repo, or tarball).

Optional environment variable: `PORT` (default `8080`).

## Known limitations

- A video with no captions (neither manual nor auto-generated) returns an
  explicit error in its result entry.
- A few protected videos return a URL flagged `exp=xpe` that requires a token
  (`Proof of Origin Token`): those are reported as such.
