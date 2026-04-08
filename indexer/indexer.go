// Package indexer handles importing PDF rulebooks into the rules-lawyer
// search index.
//
// # Indexing Pipeline
//
// Each book passes through the following stages when indexed:
//
//  1. Acquisition — IndexFromURL downloads the PDF to a temp file;
//     IndexFromFile uses a local path directly. ScanDir walks a directory
//     and calls IndexFromFile on every unindexed PDF it finds.
//
//  2. Text Extraction — ExtractPages tries two strategies in order:
//     a. pdftotext (poppler-utils) — fast, lossless for PDFs with embedded
//     text. Pages are separated by form-feed characters (\f) in the
//     pdftotext output, so each \f-delimited segment becomes one PageText.
//     b. OCR fallback — if pdftotext yields no meaningful text (fewer than
//     5 words per page), pdftoppm renders every page to a 200 DPI PNG
//     image and tesseract performs OCR on each image individually.
//     Pages that fail OCR are skipped rather than aborting the whole book.
//
//  3. Edition Detection — DetectEdition scans the combined text of the first
//     three pages for known edition markers (e.g. "pathfinder second edition",
//     "5th edition") and returns a normalised tag such as "5e2014" or
//     "pathfinder2e". Returns "unknown" when no marker is recognised.
//
//  4. Chunking — ChunkPages splits each page's text into segments of roughly
//     400 words. The splitter prefers to break at sentence-ending punctuation
//     (. ? !) so that each chunk stays semantically coherent. Every chunk
//     records the source page number so citations can be generated later.
//
//  5. Storage — The book record and all chunks are written to the SQLite FTS5
//     database via store.AddBook / store.AddChunks. The FTS5 index enables
//     full-text search with optional edition filtering.
//
// # Prerequisites
//
// pdftotext (poppler-utils) must be installed for PDFs with embedded text.
// pdftoppm (poppler-utils) and tesseract-ocr must also be installed to
// index scanned (image-only) PDFs via OCR.
package indexer

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rules-laywer/store"
)

// httpClient is used for all outbound HTTP requests with a conservative
// timeout. PDF downloads can be large, so 5 minutes is intentionally generous.
var httpClient = &http.Client{Timeout: 5 * time.Minute}

// IndexFromURL downloads a PDF from the given URL and indexes it.
// progress may be nil.
func IndexFromURL(url, bookName, forceEdition string, s *store.Store, progress ProgressFunc) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}

	progress("Downloading PDF...")
	tmp, err := os.CreateTemp("", "ruleslawyer-*.pdf")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	resp, err := httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("download pdf: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download pdf: HTTP %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return "", fmt.Errorf("write temp file: %w", err)
	}
	tmp.Close()

	return IndexFromFile(tmp.Name(), bookName, forceEdition, s, progress)
}

// IndexFromFile indexes a PDF from a local file path.
// bookName defaults to the filename stem if empty.
// forceEdition overrides auto-detection if non-empty.
// progress may be nil.
func IndexFromFile(path, bookName, forceEdition string, s *store.Store, progress ProgressFunc) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if bookName == "" {
		bookName = stemName(path)
	}

	// Check for duplicate
	exists, err := s.BookExists(bookName)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("book %q is already indexed", bookName)
	}

	// Extract pages
	pages, err := ExtractPages(path, progress)
	if err != nil {
		return "", fmt.Errorf("extract pages: %w", err)
	}
	if len(pages) == 0 {
		return "", fmt.Errorf("no text found in PDF (is it a scanned image?)")
	}

	// Detect or use forced edition
	edition := forceEdition
	if edition == "" {
		progress("Detecting edition...")
		edition = DetectEdition(pages)
	}

	// Register book
	bookID, err := s.AddBook(bookName, filepath.Base(path), edition)
	if err != nil {
		return "", fmt.Errorf("add book: %w", err)
	}

	// Chunk and store
	progress(fmt.Sprintf("Storing %d pages into index...", len(pages)))
	rawChunks := ChunkPages(pages, 400)
	storeChunks := make([]store.Chunk, len(rawChunks))
	for i, c := range rawChunks {
		storeChunks[i] = store.Chunk{
			BookName: bookName,
			Edition:  edition,
			Page:     c.Page,
			Section:  c.Section,
			Content:  c.Content,
		}
	}

	if err := s.AddChunks(bookID, bookName, edition, storeChunks); err != nil {
		s.RemoveBook(bookName) //nolint:errcheck
		return "", fmt.Errorf("add chunks: %w", err)
	}

	return edition, nil
}

// ScanDir indexes all PDFs in dir that are not already in the store.
// progress receives per-book status updates. progress may be nil.
func ScanDir(dir string, s *store.Store, progress ProgressFunc) (added []string, errs []error) {
	if progress == nil {
		progress = func(string) {}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("read dir %s: %w", dir, err)}
	}

	// Count PDFs to index for progress display
	var pdfs []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".pdf") {
			pdfs = append(pdfs, e)
		}
	}

	for n, e := range pdfs {
		bookName := stemName(e.Name())

		exists, err := s.BookExists(bookName)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		if exists {
			continue
		}

		progress(fmt.Sprintf("[%d/%d] Indexing: %s", n+1, len(pdfs), bookName))
		path := filepath.Join(dir, e.Name())
		edition, err := IndexFromFile(path, bookName, "", s, progress)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		added = append(added, fmt.Sprintf("%s (%s)", bookName, edition))
	}
	return added, errs
}

// stemName returns the filename without its extension.
func stemName(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}
