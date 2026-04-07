package store

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

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
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS books (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			name      TEXT NOT NULL UNIQUE,
			filename  TEXT NOT NULL,
			edition   TEXT NOT NULL DEFAULT 'unknown',
			added_at  DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE VIRTUAL TABLE IF NOT EXISTS chunks USING fts5(
			book_name,
			content,
			book_id   UNINDEXED,
			page      UNINDEXED,
			edition   UNINDEXED
		);
	`)
	return err
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

// AddChunks bulk-inserts chunks for a book.
func (s *Store) AddChunks(bookID int64, bookName, edition string, chunks []Chunk) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO chunks (book_name, content, book_id, page, edition)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, c := range chunks {
		if _, err := stmt.Exec(bookName, c.Content, bookID, c.Page, edition); err != nil {
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
			SELECT book_name, edition, page, content
			FROM chunks
			WHERE chunks MATCH ? AND edition = ?
			ORDER BY rank
			LIMIT ?
		`, ftsQuery, edition, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT book_name, edition, page, content
			FROM chunks
			WHERE chunks MATCH ?
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
		if err := rows.Scan(&c.BookName, &c.Edition, &c.Page, &c.Content); err != nil {
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
