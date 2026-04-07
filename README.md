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

```bash
cp .env.example .env
```

Edit `.env`:

```env
DISCORD_TOKEN=your_discord_bot_token
DISCORD_GUILD_ID=your_server_id          # optional but recommended for dev
ANTHROPIC_API_KEY=your_anthropic_key
ADMIN_ROLE_NAME=DM                       # Discord role allowed to manage books
DB_PATH=./rules.db
PDF_DIR=./pdfs
```

> **`DISCORD_GUILD_ID`** — If set, slash commands register instantly on that server. If empty, they register globally (up to 1 hour propagation delay).

### 2. Build and run

```bash
go mod tidy
go build -o rules-lawyer .
./rules-lawyer
```

### 3. Invite the bot

When creating your bot in the Discord Developer Portal, enable:
- **Bot** scope
- **`applications.commands`** scope (for slash commands)
- **Message Content Intent** (for prefix commands)

Required permissions: `Send Messages`, `Read Message History`.

---

## Adding rulebooks

### Option A — Drop PDFs in the `./pdfs/` directory

The bot scans this directory on startup and indexes any PDFs not already in the database.

```bash
mkdir -p pdfs
cp "Players_Handbook_2024.pdf" pdfs/
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
- The SQLite database (`rules.db`) is a single file — back it up to preserve your indexed books.
