package bot

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"rules-laywer/indexer"
)

const prefix = "!"

// onInteraction handles slash command interactions.
func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	data := i.ApplicationCommandData()

	// Acknowledge immediately so Discord doesn't time out (deferred response)
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("error: defer interaction: %v", err)
		return
	}

	var response string

	switch data.Name {
	case "ask":
		question := optString(data.Options, "question")
		edition := optString(data.Options, "edition")
		response = b.cmdAsk(question, edition)

	case "books":
		response = b.cmdBooks()

	case "upload":
		if !b.isAdmin(s, i.GuildID, i.Member) {
			response = "You don't have permission to upload books. See `config.yaml` to configure admin access."
			break
		}
		editionFlag := optString(data.Options, "edition")
		url := optString(data.Options, "url")
		name := optString(data.Options, "name")

		pdfPath := ""
		if i.Message != nil && len(i.Message.Attachments) > 0 {
			att := i.Message.Attachments[0]
			if strings.EqualFold(filepath.Ext(att.Filename), ".pdf") {
				var dlErr error
				pdfPath, dlErr = downloadAttachment(att.URL, att.Filename)
				if dlErr != nil {
					response = fmt.Sprintf("Failed to download attachment: %v", dlErr)
					break
				}
				defer os.Remove(pdfPath)
				if name == "" {
					name = strings.TrimSuffix(att.Filename, filepath.Ext(att.Filename))
				}
			}
		}

		response = b.runWithLiveProgress(s, i, func(progress indexer.ProgressFunc) string {
			return b.cmdUpload(url, pdfPath, name, editionFlag, progress)
		})

	case "scan":
		if !b.isAdmin(s, i.GuildID, i.Member) {
			response = "You don't have permission to scan for books. See `config.yaml` to configure admin access."
			break
		}
		response = b.runWithLiveProgress(s, i, func(progress indexer.ProgressFunc) string {
			return b.cmdScan(progress)
		})

	case "remove":
		if !b.isAdmin(s, i.GuildID, i.Member) {
			response = "You don't have permission to remove books. See `config.yaml` to configure admin access."
			break
		}
		response = b.cmdRemove(optString(data.Options, "book"))

	default:
		response = "Unknown command."
	}

	if response == "" {
		response = "(no response)"
	}
	editInteraction(s, i, response)
}

// onMessage handles prefix-based commands (e.g. !ask, !books).
// Requires the MESSAGE_CONTENT privileged intent to be enabled in the
// Discord Developer Portal; if not enabled, m.Content will be empty and
// this handler is a no-op.
func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	if m.Content == "" || !strings.HasPrefix(m.Content, prefix) {
		return
	}

	content := strings.TrimPrefix(m.Content, prefix)
	parts := strings.SplitN(content, " ", 2)
	cmd := strings.ToLower(strings.TrimSpace(parts[0]))
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	var response string

	switch cmd {
	case "ask":
		question, edition := parseEditionPrefix(args)
		response = b.cmdAsk(question, edition)

	case "books":
		response = b.cmdBooks()

	case "upload":
		if !b.isAdminByMessage(s, m) {
			response = "You don't have permission to upload books. See `config.yaml` to configure admin access."
			break
		}
		editionFlag, name, url := parseUploadArgs(args)
		pdfPath := ""
		if len(m.Attachments) > 0 {
			att := m.Attachments[0]
			if strings.EqualFold(filepath.Ext(att.Filename), ".pdf") {
				var dlErr error
				pdfPath, dlErr = downloadAttachment(att.URL, att.Filename)
				if dlErr != nil {
					response = fmt.Sprintf("Failed to download attachment: %v", dlErr)
					break
				}
				defer os.Remove(pdfPath)
				if name == "" {
					name = strings.TrimSuffix(att.Filename, filepath.Ext(att.Filename))
				}
			}
		}
		// Send a placeholder then update it when done
		sent, sendErr := s.ChannelMessageSend(m.ChannelID, "Indexing...")
		if sendErr != nil {
			log.Printf("error sending progress message: %v", sendErr)
		}
		response = b.cmdUpload(url, pdfPath, name, editionFlag, func(msg string) {
			if sent != nil {
				content := "Indexing... " + msg
				s.ChannelMessageEdit(m.ChannelID, sent.ID, content) //nolint:errcheck
			}
		})
		if sent != nil {
			s.ChannelMessageEdit(m.ChannelID, sent.ID, response) //nolint:errcheck
			return
		}

	case "scan":
		if !b.isAdminByMessage(s, m) {
			response = "You don't have permission to scan for books. See `config.yaml` to configure admin access."
			break
		}
		sent, sendErr := s.ChannelMessageSend(m.ChannelID, "Scanning...")
		if sendErr != nil {
			log.Printf("error sending progress message: %v", sendErr)
		}
		response = b.cmdScan(func(msg string) {
			if sent != nil {
				content := "Scanning... " + msg
				s.ChannelMessageEdit(m.ChannelID, sent.ID, content) //nolint:errcheck
			}
		})
		if sent != nil {
			s.ChannelMessageEdit(m.ChannelID, sent.ID, response) //nolint:errcheck
			return
		}

	case "remove":
		if !b.isAdminByMessage(s, m) {
			response = "You don't have permission to remove books. See `config.yaml` to configure admin access."
			break
		}
		response = b.cmdRemove(args)

	default:
		return
	}

	if response == "" {
		return
	}
	if _, err := s.ChannelMessageSend(m.ChannelID, truncate(response)); err != nil {
		log.Printf("error: send message: %v", err)
	}
}

// runWithLiveProgress runs fn in a goroutine, updating the Discord deferred
// interaction response every 2 seconds until fn completes.
func (b *Bot) runWithLiveProgress(s *discordgo.Session, i *discordgo.InteractionCreate, fn func(indexer.ProgressFunc) string) string {
	var (
		mu      sync.Mutex
		current = "Starting..."
	)

	progress := func(msg string) {
		mu.Lock()
		current = msg
		mu.Unlock()
	}

	resultCh := make(chan string, 1)
	go func() {
		resultCh <- fn(progress)
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mu.Lock()
			msg := truncate("⏳ " + current)
			mu.Unlock()
			editInteraction(s, i, msg)
		case result := <-resultCh:
			return result
		}
	}
}

// editInteraction edits a deferred slash command response.
func editInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	content = truncate(content)
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content,
	}); err != nil {
		log.Printf("error: edit interaction response: %v", err)
	}
}

// truncate caps a string at Discord's 2000-character message limit.
func truncate(s string) string {
	if len(s) > 1990 {
		return s[:1987] + "..."
	}
	return s
}

// isAdmin checks if the member has admin access via user ID, role ID, or role name.
func (b *Bot) isAdmin(s *discordgo.Session, guildID string, member *discordgo.Member) bool {
	if member == nil || member.User == nil {
		return false
	}

	for _, uid := range b.admin.UserIDs {
		if uid == member.User.ID {
			return true
		}
	}

	for _, roleID := range member.Roles {
		for _, adminRoleID := range b.admin.RoleIDs {
			if roleID == adminRoleID {
				return true
			}
		}
		role, err := s.State.Role(guildID, roleID)
		if err != nil {
			continue
		}
		for _, name := range b.admin.RoleNames {
			if strings.EqualFold(role.Name, name) {
				return true
			}
		}
	}
	return false
}

// isAdminByMessage resolves the member from a message and checks admin status.
func (b *Bot) isAdminByMessage(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if m.Member == nil {
		return false
	}
	return b.isAdmin(s, m.GuildID, m.Member)
}

// optString safely extracts a string option value by name.
func optString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			return o.StringValue()
		}
	}
	return ""
}

// parseEditionPrefix parses "edition:5e2024 rest of question" from ask args.
func parseEditionPrefix(args string) (question, edition string) {
	if strings.HasPrefix(args, "edition:") {
		parts := strings.SplitN(args, " ", 2)
		edition = strings.TrimPrefix(parts[0], "edition:")
		if len(parts) > 1 {
			question = parts[1]
		}
	} else {
		question = args
	}
	return
}

// parseUploadArgs parses "edition:<tag> name:<name> url:<url>" from upload args.
func parseUploadArgs(args string) (edition, name, url string) {
	for _, part := range strings.Fields(args) {
		switch {
		case strings.HasPrefix(part, "edition:"):
			edition = strings.TrimPrefix(part, "edition:")
		case strings.HasPrefix(part, "name:"):
			name = strings.TrimPrefix(part, "name:")
		case strings.HasPrefix(part, "url:"):
			url = strings.TrimPrefix(part, "url:")
		}
	}
	return
}

// downloadAttachment downloads a Discord attachment to a temp file and returns its path.
func downloadAttachment(url, filename string) (string, error) {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp("", "ruleslawyer-*-"+filename)
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}
