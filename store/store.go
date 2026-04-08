package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite"
)

// currentSchemaVersion is bumped whenever a breaking schema change is made.
// The migrate() function runs all pending migrations in order.
const currentSchemaVersion = 2

type Book struct {
	ID       int64
	Name     string
	Filename string
	Edition  string
	AddedAt  string
}

type Chunk struct {
	BookName string
	Edition  string
	Page     int
	Section  string // current section heading (may be empty)
	Content  string
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	// Schema version tracking table
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL DEFAULT 0)`); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM schema_version)`); err != nil {
		return err
	}

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		return err
	}

	// v1: initial schema — books table + basic FTS5 chunks
	if version < 1 {
		if _, err := db.Exec(`
			CREATE TABLE IF NOT EXISTS books (
				id        INTEGER PRIMARY KEY AUTOINCREMENT,
				name      TEXT NOT NULL UNIQUE,
				filename  TEXT NOT NULL,
				edition   TEXT NOT NULL DEFAULT 'unknown',
				added_at  DATETIME DEFAULT CURRENT_TIMESTAMP
			);
		`); err != nil {
			return err
		}
		if _, err := db.Exec(`
			CREATE VIRTUAL TABLE IF NOT EXISTS chunks USING fts5(
				book_name,
				content,
				book_id   UNINDEXED,
				page      UNINDEXED,
				edition   UNINDEXED
			);
		`); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE schema_version SET version = 1`); err != nil {
			return err
		}
		version = 1
	}

	// v2: better FTS5 tokenizer (unicode61 + remove_diacritics).
	// The old chunks are dropped — books with bad OCR text must be re-indexed.
	if version < 2 {
		log.Println("store: migrating to schema v2 — dropping old chunks (re-index all books)")
		if _, err := db.Exec(`DROP TABLE IF EXISTS chunks`); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE schema_version SET version = 2`); err != nil {
			return err
		}
		version = 2
	}

	// v3: replace monolithic FTS5 table with a real chunks table + FTS5 content
	// table backed by it. Adds section column. Books table is preserved.
	if version < 3 {
		log.Println("store: migrating to schema v3 — structured chunks + FTS5 content table (re-index all books)")
		if _, err := db.Exec(`DROP TABLE IF EXISTS chunks`); err != nil {
			return err
		}
		if _, err := db.Exec(`DROP TABLE IF EXISTS chunks_fts`); err != nil {
			return err
		}
		if _, err := db.Exec(`
			CREATE TABLE IF NOT EXISTS chunks (
				id        INTEGER PRIMARY KEY AUTOINCREMENT,
				book_id   INTEGER NOT NULL,
				book_name TEXT    NOT NULL,
				edition   TEXT    NOT NULL,
				page      INTEGER NOT NULL,
				section   TEXT    NOT NULL DEFAULT '',
				content   TEXT    NOT NULL
			);
		`); err != nil {
			return err
		}
		if _, err := db.Exec(`
			CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
				content,
				section,
				book_name,
				content     = 'chunks',
				content_rowid = 'id',
				tokenize    = 'unicode61 remove_diacritics 1'
			);
		`); err != nil {
			return err
		}
		if _, err := db.Exec(`
			CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
				INSERT INTO chunks_fts(rowid, content, section, book_name)
				VALUES (new.id, new.content, new.section, new.book_name);
			END;
		`); err != nil {
			return err
		}
		if _, err := db.Exec(`
			CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
				INSERT INTO chunks_fts(chunks_fts, rowid, content, section, book_name)
				VALUES ('delete', old.id, old.content, old.section, old.book_name);
			END;
		`); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE schema_version SET version = 3`); err != nil {
			return err
		}
	}

	return nil
}

// BookExists returns true if a book with the given name is already indexed.
func (s *Store) BookExists(name string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM books WHERE name = ?`, name).Scan(&count)
	return count > 0, err
}

// AddBook inserts a book record and returns its ID.
func (s *Store) AddBook(name, filename, edition string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO books (name, filename, edition) VALUES (?, ?, ?)`,
		name, filename, edition,
	)
	if err != nil {
		return 0, fmt.Errorf("insert book: %w", err)
	}
	return res.LastInsertId()
}

// AddChunks bulk-inserts chunks for a book into the chunks table.
// The chunks_fts FTS5 table is kept in sync automatically via triggers.
func (s *Store) AddChunks(bookID int64, bookName, edition string, chunks []Chunk) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO chunks (book_id, book_name, edition, page, section, content)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range chunks {
		if _, err := stmt.Exec(bookID, bookName, edition, c.Page, c.Section, c.Content); err != nil {
			return fmt.Errorf("insert chunk p%d: %w", c.Page, err)
		}
	}
	return tx.Commit()
}

// SearchChunks performs an FTS5 search and returns the top results.
// If edition is non-empty, results are filtered to that edition.
// Strategy: try AND (all words must match), fall back to OR (any word matches)
// so that conversational questions like "what is a saving throw?" still find results.
func (s *Store) SearchChunks(query, edition string, limit int) ([]Chunk, error) {
	andQuery := sanitizeFTS(query, "AND")
	orQuery := sanitizeFTS(query, "OR")

	results, err := s.runSearch(andQuery, edition, limit)
	if err != nil {
		return nil, err
	}
	if len(results) > 0 {
		return results, nil
	}
	// AND matched nothing — widen to OR
	return s.runSearch(orQuery, edition, limit)
}

func (s *Store) runSearch(ftsQuery, edition string, limit int) ([]Chunk, error) {
	var (
		rows *sql.Rows
		err  error
	)

	if edition != "" {
		rows, err = s.db.Query(`
			SELECT c.book_name, c.edition, c.page, c.section, c.content
			FROM chunks c
			JOIN chunks_fts f ON c.id = f.rowid
			WHERE chunks_fts MATCH ? AND c.edition = ?
			ORDER BY rank
			LIMIT ?
		`, ftsQuery, edition, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT c.book_name, c.edition, c.page, c.section, c.content
			FROM chunks c
			JOIN chunks_fts f ON c.id = f.rowid
			WHERE chunks_fts MATCH ?
			ORDER BY rank
			LIMIT ?
		`, ftsQuery, limit)
	}
	if err != nil {
		// FTS5 syntax errors should not propagate as hard errors
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()

	var results []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(&c.BookName, &c.Edition, &c.Page, &c.Section, &c.Content); err != nil {
			return nil, err
		}
		results = append(results, c)
	}
	return results, rows.Err()
}

// ListBooks returns all indexed books.
func (s *Store) ListBooks() ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, name, filename, edition, added_at FROM books ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var books []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Name, &b.Filename, &b.Edition, &b.AddedAt); err != nil {
			return nil, err
		}
		books = append(books, b)
	}
	return books, rows.Err()
}

// RemoveBook deletes a book and all its chunks by name.
func (s *Store) RemoveBook(name string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Get book ID first
	var id int64
	err = tx.QueryRow(`SELECT id FROM books WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// Delete chunks — the chunks_ad trigger removes them from chunks_fts automatically.
	if _, err := tx.Exec(`DELETE FROM chunks WHERE book_id = ?`, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM books WHERE id = ?`, id); err != nil {
		return false, err
	}

	return true, tx.Commit()
}

// sanitizeFTS builds an FTS5 query joining all words with the given operator ("AND" or "OR").
// All non-alphanumeric characters are replaced with spaces to prevent FTS5 syntax errors.
func sanitizeFTS(q, op string) string {
	q = strings.TrimSpace(q)
	// Replace anything that isn't a letter, digit, or space with a space
	var sb strings.Builder
	for _, r := range q {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ' ' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte(' ')
		}
	}
	words := strings.Fields(sb.String())
	if len(words) == 0 {
		return `""`
	}
	return strings.Join(words, " "+op+" ")
}
