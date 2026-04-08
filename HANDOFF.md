# Rules Lawyer — Agent Handoff Document

> **Last updated:** 2026-04-08  
> **Codebase size:** ~2,200 lines of Go across 9 source files  
> **Module name:** `rules-laywer` *(note the intentional typo — this is the Go module name, baked into all import paths)*

---

## 1. What This Project Is

A Discord bot that ingests tabletop RPG rulebooks (PDFs), indexes their text into a local SQLite full-text-search (FTS5) database, and answers rules questions by retrieving relevant excerpts and feeding them to Claude (Anthropic) with a strict "rules lawyer" system prompt. No full PDFs are ever sent to the API — only the most relevant chunks.

**Key value proposition:** Users ask a natural-language question in Discord, and the bot cites the exact page, book, and edition.

---

## 2. Architecture Overview

```
Discord User
    │
    ▼
┌──────────────────────┐
│  bot/handlers.go     │  ← Event routing: slash commands + "!" prefix commands
│  bot/bot.go          │  ← Discord session setup, slash command registration
│  bot/commands.go     │  ← Pure command logic (ask, books, upload, scan, remove, reindex)
└──────────┬───────────┘
           │
    ┌──────┴──────┐
    ▼              ▼
┌─────────┐  ┌───────────┐
│ claude/  │  │ indexer/  │
│client.go │  │indexer.go │  ← Orchestrates file/URL → parse → store
│          │  │pdf.go     │  ← PDF extraction, OCR, chunking, edition detection
└────┬─────┘  └─────┬─────┘
     │              │
     └──────┬───────┘
            ▼
      ┌───────────┐
      │ store/    │
      │ store.go  │  ← SQLite FTS5 database (schema, migrations, search, CRUD)
      └───────────┘
```

### Data Flow — Asking a Question

1. User sends `/ask How does multiattack work?` in Discord
2. `bot/handlers.go:44-48` routes to `cmdAsk()` (`bot/commands.go:14`)
3. `cmdAsk()` calls `store.SearchChunks()` — FTS5 search with AND-then-OR fallback (`store/store.go:332-345`)
4. Top 10 chunks are passed to `claude.Ask()` (`claude/client.go:27`)
5. Claude responds using only the provided excerpts with page citations
6. Response is truncated to 1990 chars and sent back to Discord

### Data Flow — Indexing a PDF

1. PDF arrives via `/upload` (attachment or URL) or `/scan` (directory scan)
2. `indexer/indexer.go:95` orchestrates: extract → detect edition → chunk → store
3. Text extraction (`indexer/pdf.go:88-113`): tries `pdftotext` first, falls back to OCR (pdftoppm + tesseract)
4. Edition detection (`indexer/pdf.go:289-318`): regex heuristics on first 3 pages
5. Chunking (`indexer/pdf.go:327-373`): ~400 words per chunk, respects paragraph boundaries and tracks section headings
6. Storage: `store.AddBook()` + `store.AddChunks()` — FTS5 index auto-updated via SQLite triggers

---

## 3. File-by-File Reference

### `main.go` (78 lines)
Entry point. Parses `--data-dir` flag, loads config, opens the store, optionally scans the PDF directory on startup, starts the Discord bot, and blocks on SIGINT/SIGTERM.

- **Line 16:** `--data-dir` defaults to `./rules-laywer-data`
- **Line 22:** Sets `indexer.OCRWorkers` from config (package-level var)
- **Line 38-45:** Optional startup scan of the PDF directory (`cfg.ScanOnStartup`)

### `config.go` (178 lines)
Configuration loading with a 3-tier precedence: environment variables > `.env` file > `config.yaml`.

- **Lines 53-88:** `LoadConfig()` — merges all config sources. Fatal if `DISCORD_TOKEN` or `ANTHROPIC_API_KEY` are missing.
- **Lines 92-112:** `loadYAMLConfig()` — reads `<dataDir>/config.yaml`; auto-creates a default one if missing.
- **Lines 114-118:** Default admin config grants the "DM" role admin access.
- **Key nuance:** `AdminConfig` is defined in both `config.go` and `bot/bot.go` (duplicated struct to keep packages independent). Values are copied from one to the other in `main.go:52-56`.

### `bot/bot.go` (190 lines)
Discord session setup and slash command definitions.

- **Lines 30-123:** `slashCommands` array — defines all 6 slash commands: `ask`, `books`, `upload`, `scan`, `remove`, `reindex`.
- **Line 126:** `New()` — creates Bot, wires handlers (`onReady`, `onInteraction`, `onMessage`).
- **Line 148:** Intents: `GuildMessages` + `MessageContent` (privileged intent required for `!` prefix commands).
- **Lines 164-180:** `onReady()` → `registerSlashCommands()` — uses `ApplicationCommandBulkOverwrite` for idempotent registration.
- **Lines 184-190:** `inviteURL()` — builds OAuth2 invite URL with minimal permissions (Send Messages + Read History + Embed Links).
- **Key nuance:** `b.guildID` controls whether commands are guild-scoped (instant) or global (up to 1hr propagation). Empty string = global.

### `bot/commands.go` (136 lines)
Pure business logic for each command, decoupled from Discord plumbing.

- **Line 12:** `searchLimit = 10` — number of FTS5 results to retrieve for Claude context.
- **Lines 14-34:** `cmdAsk()` — search → Claude API → format response.
- **Lines 37-52:** `cmdBooks()` — lists all indexed books with name, edition, date.
- **Lines 57-80:** `cmdUpload()` — delegates to `indexer.IndexFromURL` or `IndexFromFile`.
- **Lines 84-105:** `cmdScan()` — delegates to `indexer.ScanDir`.
- **Lines 108-122:** `cmdRemove()` — delegates to `store.RemoveBook`.
- **Lines 126-136:** `cmdReindex()` — destructive! Calls `RemoveAllBooks()` then `cmdScan()`.

### `bot/handlers.go` (436 lines)
Discord event routing — the largest file. Handles both slash commands and `!` prefix commands.

- **Line 19:** Prefix is `!` (hardcoded constant).
- **Lines 26-126:** `onInteraction()` — slash command handler. **Always defers the response first** (line 34) to avoid Discord's 3-second timeout, then processes the command.
- **Lines 54-84:** Upload slash command flow — resolves attachment via `data.Resolved.Attachments`, checks for `.pdf` extension, saves locally via `saveAttachment()`.
- **Lines 107-117:** Reindex confirmation — requires 3 separate confirmations (two booleans + typing "REINDEX").
- **Lines 132-232:** `onMessage()` — prefix command handler. Parses `!cmd args` format.
- **Lines 236-267:** `runWithLiveProgress()` — runs long operations in a goroutine, updates the Discord deferred response every 2 seconds with a ⏳ spinner.
- **Lines 281-292:** `truncate()` — caps at 1990 chars on a rune boundary (UTF-8 safe).
- **Lines 295-349:** `isAdmin()` / `isAdminByMessage()` — checks user ID, role ID, or role name (case-insensitive) against the admin config. Logs every check for debugging.
- **Lines 386-412:** `parseEditionPrefix()` / `parseUploadArgs()` — parse `edition:`, `name:`, `url:` key-value pairs from prefix command args.
- **Lines 417-436:** `saveAttachment()` — downloads Discord attachment to `pdfDir`. Overwrites existing files (intentional, to allow re-uploading corrected PDFs).

### `indexer/indexer.go` (210 lines)
Orchestration layer for the indexing pipeline.

- **Line 56:** `httpClient` with 5-minute timeout for PDF downloads.
- **Lines 60-89:** `IndexFromURL()` — downloads PDF to temp file, delegates to `IndexFromFile`.
- **Lines 95-153:** `IndexFromFile()` — the core pipeline: duplicate check → extract pages → detect edition → register book → chunk → store. If chunk storage fails, the book record is cleaned up (line 149).
- **Lines 157-203:** `ScanDir()` — walks a directory, skips already-indexed books by name, indexes new PDFs. Reports progress with `[n/total] BookName: status...` format.
- **Key nuance:** Book deduplication is by name only (`store.BookExists`). The v4 schema allows same name with different editions at the DB level, but the indexer code still checks by name alone.

### `indexer/pdf.go` (407 lines)
PDF text extraction, OCR, text normalization, edition detection, and chunking.

- **Lines 16-35:** Compiled regexes for text normalization and heading detection.
- **Lines 37-63:** `normalizeText()` — cleans OCR artifacts: line-ending normalization, soft-hyphen rejoining, character-spaced text collapsing (e.g., "G O O D" → "GOOD"), multi-space collapsing, per-line trimming.
- **Lines 88-113:** `ExtractPages()` — two-strategy extraction:
  1. `pdftotext` (fast, for PDFs with embedded text). Pages split on `\f` (form-feed).
  2. OCR fallback via `pdftoppm` (300 DPI PNG) + `tesseract` (OEM 3, PSM 3, English). Used when pdftotext yields <5 words/page or fragmented text.
- **Lines 136-137:** `OCRWorkers` — package-level var set from config. Controls parallel OCR goroutine count. Default 4.
- **Lines 142-197:** `extractWithOCR()` — concurrent OCR with semaphore-based worker pool. Results assembled in page order; failed pages are silently skipped.
- **Lines 268-285:** `textQualityOK()` — heuristic to detect fragmented PDF text. If >20% of tokens are single characters, the text is considered fragmented and OCR is preferred. Samples up to 5000 words.
- **Lines 289-318:** `DetectEdition()` — cascading `switch` on the first 3 pages of lowercase text. Matches keywords like "pathfinder second edition", "5th edition", "2024", etc. Falls through to "5e2014" as default for any D&D content, then "unknown".
  - **Nuance:** The detection order matters! "5e2024" checks come before the generic "dungeons & dragons" fallback. If a 2024 book doesn't contain "2024" in its first 3 pages, it'll be misdetected as "5e2014".
- **Lines 327-373:** `ChunkPages()` — paragraph-aware chunking. Tracks a running `currentSection` (detected via `isHeading()`). Flushes when word count exceeds `wordsPerChunk` (default 400). Section headings are stored separately in `Section` field for boosted FTS5 weighting.
- **Lines 396-407:** `isHeading()` — matches ALL-CAPS lines (≤10 words, ≥3 chars) or structural keywords (Chapter, Appendix, Part, Section, Step + number).

### `store/store.go` (490 lines)
SQLite database with FTS5 full-text search. The most complex file.

- **Line 14:** `currentSchemaVersion = 4` — the schema has been migrated 4 times.
- **Lines 36-62:** `Open()` — opens SQLite with WAL mode, 5-second busy timeout, and foreign key enforcement. Runs migrations.
- **Lines 68-281:** `migrate()` — versioned migration system:
  - **v1** (line 83): Initial schema. Books table + basic FTS5 chunks table (all columns in FTS5 directly).
  - **v2** (line 114): Better FTS5 tokenizer (`unicode61 remove_diacritics 1`). Drops all chunks — requires re-index.
  - **v3** (line 127): External-content FTS5 pattern. Separate `chunks` table + `chunks_fts` virtual table backed by it. Adds `section` column. Sync triggers for insert/delete.
  - **v4** (line 186): **Current schema.** Normalizes chunks table — removes denormalized `book_name`/`edition` columns. Adds FK to books with `ON DELETE CASCADE`. Adds `idx_chunks_book_id` index. Changes books UNIQUE constraint from `(name)` to `(name, edition)` (allows same book name with different editions). Drops and recreates chunks — requires re-index.
- **Key nuance:** Every schema migration drops chunks. Users must re-index all their PDFs after upgrading across schema versions. Books table data is preserved in v3→v4 via `INSERT INTO books_new ... SELECT FROM books`.

#### Current Schema (v4)

```sql
books (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    name      TEXT NOT NULL,
    filename  TEXT NOT NULL,
    edition   TEXT NOT NULL DEFAULT 'unknown',
    added_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(name, edition)
)

chunks (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    page    INTEGER NOT NULL,
    section TEXT    NOT NULL DEFAULT '',
    content TEXT    NOT NULL
)
-- INDEX: idx_chunks_book_id ON chunks(book_id)

chunks_fts USING fts5(
    content,
    section,
    content     = 'chunks',       -- external content table
    content_rowid = 'id',
    tokenize    = 'unicode61 remove_diacritics 1'
)
-- TRIGGERS: chunks_ai (after insert), chunks_ad (after delete) keep FTS in sync
```

- **Lines 332-345:** `SearchChunks()` — two-pass search strategy: first AND (all words must match), then OR fallback (any word matches). This prevents overly broad results while still handling conversational queries.
- **Lines 347-392:** `runSearch()` — JOINs chunks → chunks_fts → books. Uses `bm25(chunks_fts, 1.0, 10.0)` for ranking — section heading matches are weighted **10x** higher than body content matches.
- **Lines 416-426:** `RemoveBook()` — deletes by name. Chunks auto-cascade via FK. FTS5 sync via `chunks_ad` trigger.
- **Lines 441-490:** `sanitizeFTS()` — FTS5 query sanitizer. Strips non-alphanumeric chars (prevents FTS5 syntax errors), removes English stop words (lines 441-458), joins remaining terms with AND or OR. Falls back to original words if all were stop words.

### `claude/client.go` (84 lines)
Claude API client with the rules-lawyer system prompt.

- **Lines 14-22:** `systemPrompt` — strict rules: cite sources as `[Book Name (edition), p.N]`, never invent rules, quote directly, be literal.
- **Line 27:** `Ask()` — short-circuits with a canned response if no chunks are found (doesn't call the API).
- **Line 37:** Uses `claude-haiku-4-5-20251001` model. Max 1024 tokens per response.
- **Lines 67-84:** `buildUserMessage()` — formats chunks as numbered `## Rulebook Excerpts` with full citations (book, edition, page, section) before the `## Question`.

---

## 4. Dependencies

| Dependency | Purpose |
|---|---|
| `github.com/bwmarrin/discordgo` | Discord bot framework (websocket, REST) |
| `github.com/anthropics/anthropic-sdk-go` | Claude API client (official Go SDK, alpha) |
| `github.com/joho/godotenv` | `.env` file loading |
| `gopkg.in/yaml.v3` | Config file parsing |
| `modernc.org/sqlite` | Pure-Go SQLite driver (no CGo required) |

**External CLI tools required at runtime:**
- `pdftotext` / `pdftoppm` / `pdfinfo` (from `poppler-utils`) — PDF text extraction and page rendering
- `tesseract` (from `tesseract-ocr`) — OCR fallback for scanned PDFs

**Go version:** 1.23+

---

## 5. Configuration & Runtime

All runtime data lives in a single directory (default `./rules-laywer-data/`):

```
rules-laywer-data/
├── .env           # Secrets: DISCORD_TOKEN, ANTHROPIC_API_KEY, DISCORD_GUILD_ID
├── config.yaml    # Admin roles, OCR workers, scan_on_startup (auto-created if missing)
├── rules.db       # SQLite database (FTS5 index)
└── pdfs/          # PDF storage directory
```

**Config precedence** (highest to lowest): env var → `.env` file → `config.yaml`

**Build & Run:**
```bash
go build -o rules-lawyer .
./rules-lawyer                            # uses ./rules-laywer-data/
./rules-lawyer --data-dir /etc/rules-bot  # custom path
```

**Docker (dev):** Uses `cosmtrek/air` for hot-reload. `docker-compose.yml` mounts the repo at `/app`. The Dockerfile installs `poppler-utils` on top of the air image.

---

## 6. Known Nuances & Gotchas

1. **Module name typo:** The Go module is `rules-laywer` (misspelled "lawyer"). This is baked into all import paths (`rules-laywer/bot`, `rules-laywer/store`, etc.). The binary and repo are named `rules-lawyer` (correct spelling). Don't try to "fix" the module name without updating every import.

2. **No tests exist.** There are zero test files in the codebase. Any test infrastructure would need to be built from scratch.

3. **Schema migrations drop data.** Every migration from v1→v4 drops the `chunks` table. After upgrading, all PDFs must be re-indexed. The `books` table is preserved in most migrations, but v1→v2 is destructive for both.

4. **Admin config duplication.** `AdminConfig` is defined in both `config.go` (lines 15-19) and `bot/bot.go` (lines 14-18) as separate structs with the same fields. Values are manually copied in `main.go:52-56`. If you add a field, update both.

5. **Book deduplication.** `indexer.IndexFromFile()` checks `store.BookExists(name)` which queries by name only. But the v4 schema has `UNIQUE(name, edition)`, which means the DB allows the same book name with different editions, yet the indexer will reject it. This is a latent inconsistency.

6. **Edition detection is heuristic.** `DetectEdition()` only scans the first 3 pages and uses simple string matching. It can misdetect editions, especially for books that don't clearly state their edition upfront. The fallback for any D&D content is `5e2014`.

7. **Prefix command `!upload name:` parsing is broken for multi-word names.** `parseUploadArgs()` (handlers.go:400-412) splits on `strings.Fields()`, so `name:Players Handbook` only captures "Players". Multi-word names would need quoting support.

8. **No vendor directory committed.** The `.gitignore` excludes `vendor/` (line 10), but `CLAUDE.md` says "All Go dependencies are vendored under `vendor/`." This is a contradiction — you need to run `go mod vendor` after cloning if you want vendored deps.

9. **Reindex is slash-command-only.** The `cmdReindex()` function exists but is only wired up in `onInteraction()` (line 103-117), not in `onMessage()` (the prefix handler). This is intentional — it requires multi-option confirmations that prefix commands can't express.

10. **Live progress for long operations.** Upload, scan, and reindex use `runWithLiveProgress()` which polls every 2 seconds and edits the deferred interaction response. For prefix commands, a separate "Indexing..." message is sent and edited inline.

11. **FTS5 search has a fallback strategy.** `SearchChunks()` first tries AND (all terms must match) and falls back to OR. This means broad queries like "what is a saving throw" will still return results even though most of those words are stop-words. The stop-word list (store.go:441-458) includes common question words like "what", "how", "explain", "describe".

12. **Section heading boost.** FTS5 `bm25()` weights section matches at 10x body content matches (store.go:353-354). This means a chunk whose *heading* mentions "saving throw" will rank higher than one that merely mentions it in the body text.

13. **OCR is parallel but configurable.** `OCRWorkers` (default 4) controls concurrency via a semaphore channel in `extractWithOCR()`. Set via `ocr_workers` in `config.yaml`. Higher values speed up indexing but increase CPU/RAM usage.

14. **Text quality heuristic.** `textQualityOK()` (pdf.go:268-285) measures the ratio of single-character tokens. If >20% are single chars, the text is deemed fragmented (common with certain PDF font encodings) and OCR is used instead. This is checked after pdftotext succeeds, so some PDFs will be OCR'd even though they have embedded text.

---

## 7. Commit History Context

The repo is shallow-cloned with only 2 commits visible:

| Commit | Summary |
|---|---|
| `e2d5ca0` | "overhaul database handling in general" — checkpoint commit. Major restructuring of the database layer. |
| `40817ec` | "store: normalize schema, fix concurrency, improve search quality" — the current HEAD. WAL mode, schema v4 normalization, FK cascades, bm25 section boost, stop-word filtering, reindex command, UTF-8 safe truncation, bulk command registration. |

The codebase is in active development. The schema has evolved through 4 versions in rapid succession, suggesting the data model is still being refined.

---

## 8. Quick Reference — What to Change for Common Tasks

| Task | Files to touch |
|---|---|
| Add a new slash command | `bot/bot.go` (add to `slashCommands`), `bot/commands.go` (logic), `bot/handlers.go` (wire in `onInteraction` + optionally `onMessage`) |
| Change the Claude model or prompt | `claude/client.go:37` (model), `claude/client.go:14-22` (system prompt) |
| Add a new edition tag | `indexer/pdf.go:289-318` (add a case to `DetectEdition`) |
| Change chunk size | `indexer/indexer.go:136` (hardcoded 400), `indexer/pdf.go:327` (default fallback) |
| Modify search behavior | `store/store.go:332-392` (search strategy, bm25 weights, limit) |
| Add a config option | `config.go` (add to `yamlConfig` + `Config` structs, wire in `LoadConfig`), update `writeDefaultYAML` |
| Add an admin-only command | Same as new command + check `isAdmin()` / `isAdminByMessage()` in the handler |
| Change DB schema | `store/store.go:68-281` — add a new version block after v4, bump `currentSchemaVersion` |
| Add OCR language support | `indexer/pdf.go:227` — change `-l eng` to include additional Tesseract languages |
