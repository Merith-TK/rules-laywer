// This file implements the three PDF-processing sub-steps used by the indexer:
//   - ExtractPages  — convert a PDF to per-page text (pdftotext or OCR)
//   - DetectEdition — identify the ruleset edition from the extracted text
//   - ChunkPages    — split page text into ~400-word chunks for FTS storage

package indexer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PageText holds extracted text for a single page.
type PageText struct {
	Page    int
	Content string
}

// ProgressFunc is called during extraction to report status.
// msg is a human-readable status string suitable for display in Discord.
type ProgressFunc func(msg string)

// ExtractPages extracts text from a PDF, with two strategies in order:
//  1. pdftotext (poppler) — handles most PDFs with embedded text
//  2. OCR via pdftoppm + tesseract — for scanned image PDFs
//
// progress may be nil. Returns an error only if all strategies fail.
func ExtractPages(path string, progress ProgressFunc) ([]PageText, error) {
	if progress == nil {
		progress = func(string) {}
	}

	// Strategy 1: pdftotext
	progress("Extracting text...")
	pages, err := extractWithPDFToText(path)
	if err == nil && hasText(pages) {
		return pages, nil
	}

	// Strategy 2: OCR
	progress("No embedded text found — starting OCR (this may take several minutes)...")
	pages, err = extractWithOCR(path, progress)
	if err != nil {
		return nil, fmt.Errorf("all extraction methods failed (install poppler and tesseract); last error: %w", err)
	}
	if !hasText(pages) {
		return nil, fmt.Errorf("no text found after OCR — PDF may be corrupt or an unsupported format")
	}
	return pages, nil
}

// extractWithPDFToText uses pdftotext (poppler) to extract text.
// Pages are separated by form-feed characters (\f) in the output.
func extractWithPDFToText(path string) ([]PageText, error) {
	out, err := exec.Command("pdftotext", path, "-").Output()
	if err != nil {
		return nil, fmt.Errorf("pdftotext: %w", err)
	}

	// pdftotext separates pages with \f
	rawPages := strings.Split(string(out), "\f")
	var pages []PageText
	for i, text := range rawPages {
		text = strings.TrimSpace(text)
		if text != "" {
			pages = append(pages, PageText{Page: i + 1, Content: text})
		}
	}
	return pages, nil
}

// extractWithOCR renders each PDF page to an image with pdftoppm then
// runs tesseract on each image to produce text.
func extractWithOCR(path string, progress ProgressFunc) ([]PageText, error) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return nil, fmt.Errorf("pdftoppm not found (install poppler)")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		return nil, fmt.Errorf("tesseract not found (install tesseract)")
	}

	tmpDir, err := os.MkdirTemp("", "ruleslawyer-ocr-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Render all pages as PNG images at 200 DPI
	prefix := filepath.Join(tmpDir, "page")
	if err := exec.Command("pdftoppm", "-r", "200", "-png", path, prefix).Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %w", err)
	}

	// Collect rendered page images in order
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("read temp dir: %w", err)
	}

	// Count total pages for progress reporting
	total := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
			total++
		}
	}

	var pages []PageText
	pageNum := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		pageNum++
		progress(fmt.Sprintf("OCR: page %d/%d", pageNum, total))
		imgPath := filepath.Join(tmpDir, e.Name())

		// tesseract <image> stdout — writes text to stdout
		out, err := exec.Command("tesseract", imgPath, "stdout", "-l", "eng", "--psm", "1").Output()
		if err != nil {
			// Skip pages that fail OCR rather than aborting
			continue
		}
		text := strings.TrimSpace(string(out))
		if text != "" {
			pages = append(pages, PageText{Page: pageNum, Content: text})
		}
	}
	return pages, nil
}

// hasText returns true if at least one page has non-trivial content.
func hasText(pages []PageText) bool {
	for _, p := range pages {
		if len(strings.Fields(p.Content)) > 5 {
			return true
		}
	}
	return false
}

// DetectEdition scans the first few pages of text for edition markers.
// It checks for well-known phrases in priority order:
//  1. Pathfinder 2e — "pathfinder second edition" or "pathfinder 2e"
//  2. Pathfinder 1e — "pathfinder roleplaying game" (without "second edition")
//  3. D&D 5e 2024 revised — "revised" + ("2024" or "one d&d")
//  4. D&D 5e 2024 — "dungeons & dragons" + "2024" (without "revised")
//  5. D&D 5e 2014 — ("5th edition" or "dungeons & dragons") + "2014"
//  6. D&D 4th Edition — "4th edition" or "dungeons & dragons, 4th"
//  7. D&D 3.5 Edition — "3.5" or "v.3.5"
//
// Returns a normalized edition tag (see edition tags in README) or "unknown"
// if no recognised markers are found.
func DetectEdition(pages []PageText) string {
	sample := ""
	for i, p := range pages {
		if i >= 3 {
			break
		}
		sample += strings.ToLower(p.Content)
	}

	switch {
	case contains(sample, "pathfinder second edition") || contains(sample, "pathfinder 2e"):
		return "pathfinder2e"
	case contains(sample, "pathfinder roleplaying game") && !contains(sample, "second edition"):
		return "pathfinder1e"
	case contains(sample, "dungeons & dragons") && contains(sample, "2024") && !contains(sample, "revised"):
		return "5e2024"
	case contains(sample, "revised") && (contains(sample, "2024") || contains(sample, "one d&d")):
		return "5e2024"
	case (contains(sample, "5th edition") || contains(sample, "dungeons & dragons")) && contains(sample, "2014"):
		return "5e2014"
	case contains(sample, "4th edition") || contains(sample, "dungeons & dragons, 4th"):
		return "dnd4e"
	case contains(sample, "3.5") || contains(sample, "v.3.5"):
		return "dnd3.5e"
	default:
		return "unknown"
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// ChunkPages splits page text into ~wordsPerChunk-word chunks, preserving page
// numbers. Chunk boundaries are snapped back to the nearest sentence-ending
// word (ending with `.`, `?`, or `!`) in the second half of each window to
// avoid cutting mid-sentence. Use wordsPerChunk ≤ 0 to default to 400.
func ChunkPages(pages []PageText, wordsPerChunk int) []chunkWithPage {
	if wordsPerChunk <= 0 {
		wordsPerChunk = 400
	}

	var chunks []chunkWithPage
	for _, p := range pages {
		for _, c := range chunkText(p.Content, wordsPerChunk) {
			chunks = append(chunks, chunkWithPage{Page: p.Page, Content: c})
		}
	}
	return chunks
}

type chunkWithPage struct {
	Page    int
	Content string
}

// chunkText splits text into segments of approximately maxWords words,
// breaking at sentence boundaries where possible.
func chunkText(text string, maxWords int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var chunks []string
	start := 0
	for start < len(words) {
		end := start + maxWords
		if end > len(words) {
			end = len(words)
		}
		if end < len(words) {
			for i := end; i > start+maxWords/2; i-- {
				w := words[i-1]
				if strings.HasSuffix(w, ".") || strings.HasSuffix(w, "?") || strings.HasSuffix(w, "!") {
					end = i
					break
				}
			}
		}
		chunk := strings.Join(words[start:end], " ")
		if strings.TrimSpace(chunk) != "" {
			chunks = append(chunks, chunk)
		}
		start = end
	}
	return chunks
}
