# Rules Lawyer

A Discord bot that ingests tabletop RPG rulebooks (PDF) and answers rules questions by citing the exact source text. Built with Go, SQLite FTS5, and the Claude API.

## How it works

1. **Index** — Upload a PDF rulebook. The bot extracts text page-by-page, splits it into chunks, and stores them in a local SQLite full-text-search database.
2. **Ask** — When you ask a rules question, the bot searches the index for the most relevant excerpts and sends them to Claude with a strict "rules lawyer" prompt.
3. **Cite** — Claude answers using *only* the provided text and cites the book name, edition, and page number.

No full PDFs are ever sent to the API — only the relevant excerpts.

---

## Setup

### Prerequisites

- Go 1.23+
- A [Discord bot token](https://discord.com/developers/applications)
- An [Anthropic API key](https://console.anthropic.com/)

### 1. Configure

All runtime data (database, PDFs, config) lives in a single data directory. The default is `./rules-laywer-data/`. Use `--data-dir` to point it elsewhere.

```bash
mkdir -p rules-laywer-data
cp .env.example rules-laywer-data/.env
```

**Option A — secrets in `.env`** (recommended for local use)

Edit `rules-laywer-data/.env`:

```env
DISCORD_TOKEN=your_discord_bot_token
DISCORD_GUILD_ID=your_server_id    # optional but recommended for dev
ANTHROPIC_API_KEY=your_anthropic_key
```

**Option B — tokens in `config.yaml`** (useful for container/managed deployments)

You can also set tokens directly in `config.yaml`. Environment variables and `.env` values always take priority over values defined here.

```yaml
tokens:
  discord_token: your_discord_bot_token
  discord_guild_id: ""          # optional
  anthropic_api_key: your_anthropic_api_key
```

**Admin configuration** lives in `rules-laywer-data/config.yaml` (auto-created on first run):

```yaml
admin:
  role_names:        # matched case-insensitively
    - DM
  role_ids: []       # Discord role snowflake IDs — right-click role → Copy Role ID
  user_ids: []       # specific Discord user IDs — right-click user → Copy User ID
```

> `DB_PATH` and `PDF_DIR` default to `<data-dir>/rules.db` and `<data-dir>/pdfs/`. Override them in `.env` if needed.

> **`DISCORD_GUILD_ID`** — If set, slash commands register instantly on that server. If empty, they register globally (up to 1 hour propagation delay).

### 2. Build and run

```bash
go mod tidy
go build -o rules-lawyer .
./rules-lawyer                              # uses ./rules-laywer-data/
./rules-lawyer --data-dir /etc/rules-bot   # custom data directory
```

### 3. Invite the bot

When creating your bot in the Discord Developer Portal, enable:
- **Bot** scope
- **`applications.commands`** scope (for slash commands)
- **Message Content Intent** (for prefix commands)

Required permissions: `Send Messages`, `Read Message History`.

---

## How indexing works

When you add a PDF (via `/upload`, `/scan`, or the startup scan), the following pipeline runs:

### Step 1 — Text extraction

The indexer first tries **pdftotext** (part of the [poppler](https://poppler.freedesktop.org/) suite). It outputs all page text separated by form-feed characters (`\f`), which are split into per-page records.

If pdftotext produces fewer than 5 words per page — indicating a scanned/image-only PDF — the indexer falls back to **OCR**:
1. `pdftoppm` renders each page to a PNG image at 200 DPI.
2. `tesseract` runs on each image with English language data and auto page-segmentation (`--psm 1`).

> **Note:** Both `pdftotext` and `pdftoppm`/`tesseract` must be installed on the host system. OCR can take several minutes for large books.

### Step 2 — Edition detection

The first **three pages** of extracted text are concatenated and searched for well-known markers (checked in priority order):

| Marker pattern | Detected edition |
|---|---|
| "pathfinder second edition" or "pathfinder 2e" | `pathfinder2e` |
| "pathfinder roleplaying game" (without "second edition") | `pathfinder1e` |
| "revised" + ("2024" or "one d&d") | `5e2024` |
| "dungeons & dragons" + "2024" (without "revised") | `5e2024` |
| ("5th edition" or "dungeons & dragons") + "2014" | `5e2014` |
| "4th edition" or "dungeons & dragons, 4th" | `dnd4e` |
| "3.5" or "v.3.5" | `dnd3.5e` |
| no match | `unknown` |

You can bypass auto-detection by passing `edition:<tag>` when uploading.

### Step 3 — Chunking

Each page's text is split into **~400-word chunks**. The split point is moved backwards to the nearest sentence-ending punctuation (`.`, `!`, `?`) so chunks never cut mid-sentence. Every chunk keeps its source page number for later citation.

### Step 4 — Storage

All chunks are bulk-inserted into an **SQLite FTS5** (full-text search) table in a single transaction. If the insertion fails, the book record is rolled back. The FTS5 index enables fast ranked keyword search over all stored text.

---


### Option A — Drop PDFs in the data directory

The bot scans `<data-dir>/pdfs/` on startup and indexes any PDFs not already in the database.

```bash
mkdir -p rules-laywer-data/pdfs
cp "Players_Handbook_2024.pdf" rules-laywer-data/pdfs/
./rules-lawyer   # indexes it automatically on start
```

### Option B — Upload via Discord

Use the `/upload` slash command or `!upload` prefix command. Attach a PDF to the message.

```
/upload
/upload edition:5e2024 name:Players Handbook 2024
```

### Option C — URL

```
/upload url:https://example.com/rulebook.pdf edition:5e2014
```

---

## Commands

| Command | Access | Description |
|---|---|---|
| `/ask <question>` | Everyone | Answer a rules question from all indexed books |
| `/ask edition:<tag> <question>` | Everyone | Filter answers to a specific edition |
| `/books` | Everyone | List all indexed rulebooks |
| `/upload [edition:<tag>] [name:<name>] [url:<url>]` | Admin role | Index a PDF (attach or provide URL) |
| `/scan` | Admin role | Re-scan the `PDF_DIR` directory for new PDFs |
| `/remove <book>` | Admin role | Remove a book and all its chunks from the index |

All commands are also available as prefix commands with `!` (e.g. `!ask`, `!books`).

For prefix-based ask with edition filter:
```
!ask edition:5e2024 Can I cast two spells in one turn?
```

For prefix-based upload with options:
```
!upload edition:5e2024 name:Dungeon Masters Guide 2024
```
(with a PDF attached to the message)

---

## Edition tags

The bot auto-detects the edition from the PDF text. You can also force it with `edition:<tag>`.

| Tag | Ruleset |
|---|---|
| `5e2014` | D&D 5th Edition (2014 core books) |
| `5e2024` | D&D 5.5e / 2024 revised core |
| `pathfinder2e` | Pathfinder Second Edition |
| `pathfinder1e` | Pathfinder First Edition |
| `dnd4e` | D&D 4th Edition |
| `dnd3.5e` | D&D 3.5 Edition |
| `unknown` | Could not be detected |

---

## Project structure

```
rules-laywer/
├── main.go              # Entry point: load config, start bot, scan PDF dir
├── config.go            # Environment variable config
├── bot/
│   ├── bot.go           # Discord session setup and slash command registration
│   ├── commands.go      # Command handler logic (ask, books, upload, scan, remove)
│   └── handlers.go      # Discord event routing (slash + prefix), admin checks
├── indexer/
│   ├── pdf.go           # PDF text extraction, edition detection, chunking
│   └── indexer.go       # Orchestrates file/URL → parse → store pipeline
├── store/
│   └── store.go         # SQLite FTS5 database (schema, search, CRUD)
└── claude/
    └── client.go        # Claude API client with rules-lawyer system prompt
```

---

## Notes

- **Scanned PDFs** (image-only, no embedded text) are not supported. The PDF must contain selectable text.
- Long answers are truncated to Discord's 2000-character message limit.
- The bot uses **Claude Haiku** by default for cost efficiency. You can change the model in `claude/client.go`.
- The SQLite database is a single file at `<data-dir>/rules.db` — back it up to preserve your indexed books.
- The entire `rules-laywer-data/` directory is gitignored by default to keep credentials and data out of version control.
