package bot

import (
	"fmt"
	"strings"

	"rules-laywer/claude"
	"rules-laywer/indexer"
)

const searchLimit = 5

// cmdAsk handles the /ask command and !ask prefix.
func (b *Bot) cmdAsk(question, edition string) string {
	question = strings.TrimSpace(question)
	if question == "" {
		return "Please provide a question."
	}

	chunks, err := b.store.SearchChunks(question, edition, searchLimit)
	if err != nil {
		return fmt.Sprintf("Search error: %v", err)
	}

	answer, err := claude.Ask(b.anthropicKey, question, chunks)
	if err != nil {
		return fmt.Sprintf("Claude API error: %v", err)
	}

	if edition != "" {
		return fmt.Sprintf("*Searching edition: **%s***\n\n%s", edition, answer)
	}
	return answer
}

// cmdBooks handles the /books command and !books prefix.
func (b *Bot) cmdBooks() string {
	books, err := b.store.ListBooks()
	if err != nil {
		return fmt.Sprintf("Error listing books: %v", err)
	}
	if len(books) == 0 {
		return "No rulebooks indexed yet. Use `/upload` to add one."
	}

	var sb strings.Builder
	sb.WriteString("**Indexed Rulebooks:**\n")
	for _, bk := range books {
		sb.WriteString(fmt.Sprintf("- **%s** `(%s)` — added %s\n", bk.Name, bk.Edition, bk.AddedAt[:10]))
	}
	return sb.String()
}

// cmdUpload handles the /upload command and !upload prefix.
// pdfURL and pdfPath are mutually exclusive sources.
// progress is called with status updates during indexing.
func (b *Bot) cmdUpload(pdfURL, pdfPath, bookName, forceEdition string, progress indexer.ProgressFunc) string {
	var (
		edition string
		err     error
	)

	if pdfURL != "" {
		edition, err = indexer.IndexFromURL(pdfURL, bookName, forceEdition, b.store, progress)
	} else if pdfPath != "" {
		edition, err = indexer.IndexFromFile(pdfPath, bookName, forceEdition, b.store, progress)
	} else {
		return "Please attach a PDF or provide a URL."
	}

	if err != nil {
		return fmt.Sprintf("Failed to index: %v", err)
	}

	displayName := bookName
	if displayName == "" {
		displayName = "(auto-named from file)"
	}
	return fmt.Sprintf("Indexed **%s** — detected edition: `%s`", displayName, edition)
}

// cmdScan handles the /scan command and !scan prefix.
// progress is called with status updates during indexing.
func (b *Bot) cmdScan(progress indexer.ProgressFunc) string {
	added, errs := indexer.ScanDir(b.pdfDir, b.store, progress)

	var sb strings.Builder
	if len(added) > 0 {
		sb.WriteString(fmt.Sprintf("**Indexed %d new book(s):**\n", len(added)))
		for _, name := range added {
			sb.WriteString(fmt.Sprintf("- %s\n", name))
		}
	} else {
		sb.WriteString("No new PDFs found in the PDF directory.\n")
	}

	if len(errs) > 0 {
		sb.WriteString("\n**Errors:**\n")
		for _, e := range errs {
			sb.WriteString(fmt.Sprintf("- %v\n", e))
		}
	}

	return strings.TrimSpace(sb.String())
}

// cmdRemove handles the /remove command and !remove prefix.
func (b *Bot) cmdRemove(bookName string) string {
	bookName = strings.TrimSpace(bookName)
	if bookName == "" {
		return "Please specify a book name."
	}

	removed, err := b.store.RemoveBook(bookName)
	if err != nil {
		return fmt.Sprintf("Error removing book: %v", err)
	}
	if !removed {
		return fmt.Sprintf("No book named **%s** found.", bookName)
	}
	return fmt.Sprintf("Removed **%s** from the index.", bookName)
}
