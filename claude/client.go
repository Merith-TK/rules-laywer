package claude

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"rules-laywer/store"
)

const systemPrompt = `You are a precise rules lawyer for tabletop RPGs.
Answer questions ONLY based on the rulebook excerpts provided below.

Rules you must follow:
- Always cite your source using the format [Book Name (edition), p.N]
- If the answer is not found in the provided excerpts, respond with exactly: "I could not find a rule covering this in the indexed books."
- Never invent, extrapolate, or assume rules not present in the provided text.
- Quote relevant rule text directly when it supports your answer.
- If multiple rules interact, explain how they interact based only on the provided excerpts.
- Be precise and literal — you are a rules lawyer, not a storyteller.`

// Ask sends a rules question to Claude along with retrieved chunks as context.
// It returns the model's answer.
func Ask(apiKey, question string, chunks []store.Chunk) (string, error) {
	if len(chunks) == 0 {
		return "I could not find any relevant rules in the indexed books for that question.", nil
	}

	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	userMessage := buildUserMessage(question, chunks)

	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.F[anthropic.Model]("claude-haiku-4-5-20251001"),
		MaxTokens: anthropic.F(int64(1024)),
		System: anthropic.F([]anthropic.TextBlockParam{
			{
				Type: anthropic.F(anthropic.TextBlockParamTypeText),
				Text: anthropic.F(systemPrompt),
			},
		}),
		Messages: anthropic.F([]anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userMessage)),
		}),
	})
	if err != nil {
		return "", fmt.Errorf("claude api: %w", err)
	}

	if len(msg.Content) == 0 {
		return "", fmt.Errorf("claude returned empty response")
	}

	// Content blocks are typed; find the first text block
	for _, block := range msg.Content {
		if block.Type == anthropic.ContentBlockTypeText {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("no text block in claude response")
}

// buildUserMessage formats the retrieved chunks and question into a single prompt.
func buildUserMessage(question string, chunks []store.Chunk) string {
	var sb strings.Builder

	sb.WriteString("## Rulebook Excerpts\n\n")
	for i, c := range chunks {
		citation := fmt.Sprintf("%s (%s), p.%d", c.BookName, c.Edition, c.Page)
		if c.Section != "" {
			citation += " — " + c.Section
		}
		sb.WriteString(fmt.Sprintf("**[%d] %s**\n%s\n\n", i+1, citation, c.Content))
	}

	sb.WriteString("---\n\n")
	sb.WriteString("## Question\n\n")
	sb.WriteString(question)

	return sb.String()
}
