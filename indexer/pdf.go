package indexer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	// spacedCharsRe matches character-spaced uppercase text like "G O O D" (3+ chars).
	// This is a common artifact in PDFs that use letter-spacing on headings/all-caps words.
	spacedCharsRe = regexp.MustCompile(`[A-Z](?: [A-Z]){2,}`)

	// hyphenLineBreakRe matches soft-hyphen line breaks ("{word}-\n") produced
	// when pdftotext or OCR splits a hyphenated word across two lines.
	hyphenLineBreakRe = regexp.MustCompile(`-\n`)

	// multiSpaceRe collapses multiple consecutive non-newline spaces to one.
	multiSpaceRe = regexp.MustCompile(`[^\S\n]{2,}`)

	// paraBreakRe splits text into paragraphs at two or more consecutive newlines.
	paraBreakRe = regexp.MustCompile(`\n{2,}`)

	// allCapsLineRe matches lines that are entirely uppercase (section headings).
	allCapsLineRe = regexp.MustCompile(`^[A-Z][A-Z\s\d'&:,\-\.]{2,60}$`)

	// chapterLineRe matches structural keywords like "Chapter 1", "Appendix A".
	chapterLineRe = regexp.MustCompile(`(?i)^(chapter|appendix|part|section|step)\s+[\dIVXivx]+`)
)

// normalizeText cleans common OCR and pdftotext artifacts from extracted text.
func normalizeText(text string) string {
	// Normalize line endings
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	// Re-join words split across lines by soft hyphens: "some-\nword" → "someword"
	text = hyphenLineBreakRe.ReplaceAllString(text, "")

	// Collapse character-spaced uppercase text: "G O O D" → "GOOD"
	text = spacedCharsRe.ReplaceAllStringFunc(text, func(s string) string {
		return strings.ReplaceAll(s, " ", "")
	})

	// Normalize tabs to spaces
	text = strings.ReplaceAll(text, "\t", " ")

	// Collapse multiple spaces on the same line to one
	text = multiSpaceRe.ReplaceAllString(text, " ")

	// Trim each line
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// normalizePages applies normalizeText to every page in-place.
func normalizePages(pages []PageText) []PageText {
	for i := range pages {
		pages[i].Content = normalizeText(pages[i].Content)
	}
	return pages
}

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
//  2. OCR via pdftoppm + tesseract — for scanned image PDFs or badly-encoded text
//
// progress may be nil. Returns an error only if all strategies fail.
func ExtractPages(path string, progress ProgressFunc) ([]PageText, error) {
	if progress == nil {
		progress = func(string) {}
	}

	// Strategy 1: pdftotext
	progress("Extracting text...")
	pages, err := extractWithPDFToText(path)
	if err == nil && hasText(pages) && textQualityOK(pages) {
		return normalizePages(pages), nil
	}
	if err == nil && hasText(pages) {
		progress("Embedded text appears fragmented — switching to OCR for better accuracy...")
	}

	// Strategy 2: OCR
	progress("Starting OCR (this may take several minutes)...")
	pages, err = extractWithOCR(path, progress)
	if err != nil {
		return nil, fmt.Errorf("all extraction methods failed (install poppler and tesseract); last error: %w", err)
	}
	if !hasText(pages) {
		return nil, fmt.Errorf("no text found after OCR — PDF may be corrupt or an unsupported format")
	}
	return normalizePages(pages), nil
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

// OCRWorkers controls how many pages are OCR'd in parallel.
// Set this at startup from config; default 4.
var OCRWorkers = 4

// extractWithOCR renders each PDF page to an image with pdftoppm and runs
// tesseract in parallel (up to OCRWorkers goroutines). Results are assembled
// in page order; pages that fail OCR are skipped.
func extractWithOCR(path string, progress ProgressFunc) ([]PageText, error) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return nil, fmt.Errorf("pdftoppm not found (install poppler)")
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		return nil, fmt.Errorf("tesseract not found (install tesseract)")
	}

	total, err := getPDFPageCount(path)
	if err != nil {
		return nil, fmt.Errorf("get page count: %w", err)
	}
	if total == 0 {
		return nil, fmt.Errorf("PDF reports 0 pages")
	}

	workers := OCRWorkers
	if workers <= 0 {
		workers = 4
	}

	// results[i] holds the text for page i+1 (empty = skip)
	results := make([]string, total)

	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, workers)
		done atomic.Int32
	)

	for pageNum := 1; pageNum <= total; pageNum++ {
		wg.Add(1)
		sem <- struct{}{} // acquire

		go func(pn int) {
			defer wg.Done()
			defer func() { <-sem }() // release

			text := ocrPage(path, pn)
			results[pn-1] = text

			n := done.Add(1)
			progress(fmt.Sprintf("OCR: page %d/%d (%d workers)", n, total, workers))
		}(pageNum)
	}

	wg.Wait()

	var pages []PageText
	for i, text := range results {
		if text != "" {
			pages = append(pages, PageText{Page: i + 1, Content: text})
		}
	}
	return pages, nil
}

// ocrPage renders a single PDF page to a temp PNG and runs tesseract on it.
// Returns the extracted text, or "" on any error.
func ocrPage(path string, pageNum int) string {
	pageDir, err := os.MkdirTemp("", "rl-ocr-page-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(pageDir)

	prefix := filepath.Join(pageDir, "p")
	n := strconv.Itoa(pageNum)
	if err := exec.Command(
		"pdftoppm", "-r", "300", "-png",
		"-f", n, "-l", n,
		path, prefix,
	).Run(); err != nil {
		return ""
	}

	entries, _ := os.ReadDir(pageDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".png") {
			continue
		}
		imgPath := filepath.Join(pageDir, e.Name())
		out, err := exec.Command(
			"tesseract", imgPath, "stdout",
			"--oem", "3", "--psm", "3", "-l", "eng",
		).Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		break
	}
	return ""
}

// getPDFPageCount returns the number of pages in a PDF using pdfinfo.
func getPDFPageCount(path string) (int, error) {
	out, err := exec.Command("pdfinfo", path).Output()
	if err != nil {
		return 0, fmt.Errorf("pdfinfo: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			if err == nil && n > 0 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("pdfinfo: could not parse page count")
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

// textQualityOK returns false when the extracted text appears fragmented —
// a common artifact with certain PDF font encodings where pdftotext emits
// individual characters separated by spaces (e.g. "R oll" instead of "Roll").
// It measures the ratio of single-character tokens: good English text is well
// under 15%; fragmented PDFs are often 30–60%.
func textQualityOK(pages []PageText) bool {
	var total, single int
	for _, p := range pages {
		for _, w := range strings.Fields(p.Content) {
			total++
			if len([]rune(w)) == 1 {
				single++
			}
		}
		if total > 5000 { // large enough sample
			break
		}
	}
	if total < 50 {
		return false
	}
	return float64(single)/float64(total) < 0.20
}

// DetectEdition scans the first few pages of text for edition markers.
// Returns a normalized edition tag or "unknown".
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
	case contains(sample, "dungeons & dragons") || contains(sample, "d&d"):
		return "5e2014"
	default:
		return "unknown"
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// ChunkPages splits page text into semantically-aware chunks that respect
// paragraph boundaries and track section headings. The heading is stored
// separately in Section so it can be indexed and cited independently of the body.
func ChunkPages(pages []PageText, wordsPerChunk int) []chunkWithPage {
	if wordsPerChunk <= 0 {
		wordsPerChunk = 400
	}

	var chunks []chunkWithPage
	currentSection := ""

	for _, p := range pages {
		paragraphs := splitParagraphs(p.Content)

		var pendingParas []string
		pendingWords := 0

		flush := func() {
			if len(pendingParas) == 0 {
				return
			}
			content := strings.Join(pendingParas, "\n\n")
			if strings.TrimSpace(content) != "" {
				chunks = append(chunks, chunkWithPage{
					Page:    p.Page,
					Section: currentSection,
					Content: content,
				})
			}
			pendingParas = nil
			pendingWords = 0
		}

		for _, para := range paragraphs {
			if isHeading(para) {
				flush()
				currentSection = para
				continue
			}
			wc := len(strings.Fields(para))
			if pendingWords > 0 && pendingWords+wc > wordsPerChunk {
				flush()
			}
			pendingParas = append(pendingParas, para)
			pendingWords += wc
		}
		flush()
	}
	return chunks
}

type chunkWithPage struct {
	Page    int
	Section string // current section heading (empty if none detected)
	Content string // body text only
}

// splitParagraphs splits text into paragraphs at blank-line boundaries.
func splitParagraphs(text string) []string {
	parts := paraBreakRe.Split(text, -1)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// isHeading returns true when a paragraph looks like a section heading:
// a short line (≤10 words) that is ALL-CAPS or matches a structural keyword.
func isHeading(line string) bool {
	line = strings.TrimSpace(line)
	words := strings.Fields(line)
	if len(words) == 0 || len(words) > 10 {
		return false
	}
	// Must be at least 3 characters total to avoid single-letter false positives
	if len(line) < 3 {
		return false
	}
	return allCapsLineRe.MatchString(line) || chapterLineRe.MatchString(line)
}
