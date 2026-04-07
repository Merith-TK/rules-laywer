package indexer

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// PageText holds extracted text for a single page.
type PageText struct {
	Page    int
	Content string
}

// ExtractPages extracts text from each page of a PDF file.
func ExtractPages(path string) ([]PageText, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close()

	totalPages := r.NumPage()
	pages := make([]PageText, 0, totalPages)

	for i := 1; i <= totalPages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := extractPageText(p)
		if err != nil {
			// Skip pages that fail; don't abort the whole PDF
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		pages = append(pages, PageText{Page: i, Content: text})
	}

	return pages, nil
}

func extractPageText(p pdf.Page) (string, error) {
	var buf bytes.Buffer
	content := p.Content()
	for _, text := range content.Text {
		buf.WriteString(text.S)
		buf.WriteByte(' ')
	}
	return buf.String(), nil
}

// DetectEdition scans the first few pages of text for edition markers.
// Returns a normalized edition tag or "unknown".
func DetectEdition(pages []PageText) string {
	// Combine first 3 pages for heuristic scanning
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
		// Generic D&D — default to 5e2014 as most common
		return "5e2014"
	default:
		return "unknown"
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// ChunkPages splits page text into ~400-word chunks, preserving page numbers.
func ChunkPages(pages []PageText, wordsPerChunk int) []chunkWithPage {
	if wordsPerChunk <= 0 {
		wordsPerChunk = 400
	}

	var chunks []chunkWithPage
	for _, p := range pages {
		pageChunks := chunkText(p.Content, wordsPerChunk)
		for _, c := range pageChunks {
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
		// Try to break at a sentence end (period, ?, !)
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
