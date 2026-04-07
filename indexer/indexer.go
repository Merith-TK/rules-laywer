package indexer

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"rules-laywer/store"
)

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

	resp, err := http.Get(url) //nolint:noctx
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
