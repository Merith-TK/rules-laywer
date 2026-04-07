package bot

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwmarrin/discordgo"
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
			response = "You need the **" + b.adminRoleName + "** role to upload books."
			break
		}
		edition := optString(data.Options, "edition")
		url := optString(data.Options, "url")
		name := optString(data.Options, "name")

		// Check for attachment in the originating message
		pdfPath := ""
		if i.Message != nil && len(i.Message.Attachments) > 0 {
			att := i.Message.Attachments[0]
			if strings.EqualFold(filepath.Ext(att.Filename), ".pdf") {
				pdfPath, err = downloadAttachment(att.URL, att.Filename)
				if err != nil {
					response = fmt.Sprintf("Failed to download attachment: %v", err)
					break
				}
				defer os.Remove(pdfPath)
				if name == "" {
					name = strings.TrimSuffix(att.Filename, filepath.Ext(att.Filename))
				}
			}
		}
		response = b.cmdUpload(url, pdfPath, name, edition)

	case "scan":
		if !b.isAdmin(s, i.GuildID, i.Member) {
			response = "You need the **" + b.adminRoleName + "** role to scan for books."
			break
		}
		response = b.cmdScan()

	case "remove":
		if !b.isAdmin(s, i.GuildID, i.Member) {
			response = "You need the **" + b.adminRoleName + "** role to remove books."
			break
		}
		response = b.cmdRemove(optString(data.Options, "book"))

	default:
		response = "Unknown command."
	}

	if response == "" {
		response = "(no response)"
	}

	// Discord messages have a 2000 char limit; truncate if needed
	if len(response) > 1990 {
		response = response[:1987] + "..."
	}

	_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &response,
	})
	if err != nil {
		log.Printf("error: edit interaction response: %v", err)
	}
}

// onMessage handles prefix-based commands (e.g. !ask, !books).
func (b *Bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}
	if !strings.HasPrefix(m.Content, prefix) {
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
		// Support optional edition: prefix  e.g. !ask edition:5e2024 what is grappling?
		question, edition := parseEditionPrefix(args)
		response = b.cmdAsk(question, edition)

	case "books":
		response = b.cmdBooks()

	case "upload":
		if !b.isAdminByMessage(s, m) {
			response = "You need the **" + b.adminRoleName + "** role to upload books."
			break
		}
		// Parse args: [edition:<tag>] [name:<name>] [url:<url>] or attachment
		edition, name, url := parseUploadArgs(args)
		pdfPath := ""
		if len(m.Attachments) > 0 {
			att := m.Attachments[0]
			if strings.EqualFold(filepath.Ext(att.Filename), ".pdf") {
				var err error
				pdfPath, err = downloadAttachment(att.URL, att.Filename)
				if err != nil {
					response = fmt.Sprintf("Failed to download attachment: %v", err)
					break
				}
				defer os.Remove(pdfPath)
				if name == "" {
					name = strings.TrimSuffix(att.Filename, filepath.Ext(att.Filename))
				}
			}
		}
		response = b.cmdUpload(url, pdfPath, name, edition)

	case "scan":
		if !b.isAdminByMessage(s, m) {
			response = "You need the **" + b.adminRoleName + "** role to scan for books."
			break
		}
		response = b.cmdScan()

	case "remove":
		if !b.isAdminByMessage(s, m) {
			response = "You need the **" + b.adminRoleName + "** role to remove books."
			break
		}
		response = b.cmdRemove(args)

	default:
		return // ignore unknown prefix commands
	}

	if response == "" {
		return
	}
	if len(response) > 1990 {
		response = response[:1987] + "..."
	}
	if _, err := s.ChannelMessageSend(m.ChannelID, response); err != nil {
		log.Printf("error: send message: %v", err)
	}
}

// isAdmin checks if the interaction member has the configured admin role.
func (b *Bot) isAdmin(s *discordgo.Session, guildID string, member *discordgo.Member) bool {
	if member == nil {
		return false
	}
	for _, roleID := range member.Roles {
		role, err := s.State.Role(guildID, roleID)
		if err != nil {
			continue
		}
		if strings.EqualFold(role.Name, b.adminRoleName) {
			return true
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
