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

// discordHTTPClient is used for downloading Discord attachments. 60 seconds
// is sufficient since Discord enforces its own file size caps.
var discordHTTPClient = &http.Client{Timeout: 60 * time.Second}

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
		if attID := optRawString(data.Options, "file"); attID != "" && data.Resolved != nil {
			if att, ok := data.Resolved.Attachments[attID]; ok {
				if strings.EqualFold(filepath.Ext(att.Filename), ".pdf") {
					var dlErr error
					pdfPath, dlErr = b.saveAttachment(att.URL, att.Filename)
					if dlErr != nil {
						response = fmt.Sprintf("Failed to download attachment: %v", dlErr)
						break
					}
					if name == "" {
						name = strings.TrimSuffix(att.Filename, filepath.Ext(att.Filename))
					}
				} else {
					response = "Attached file must be a PDF."
					break
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
				pdfPath, dlErr = b.saveAttachment(att.URL, att.Filename)
				if dlErr != nil {
					response = fmt.Sprintf("Failed to download attachment: %v", dlErr)
					break
				}
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
	if member == nil {
		log.Printf("isAdmin: denied — member is nil")
		return false
	}
	if member.User == nil {
		log.Printf("isAdmin: denied — member.User is nil")
		return false
	}

	log.Printf("isAdmin: checking user %s | configured userIDs=%v roleIDs=%v roleNames=%v",
		member.User.ID, b.admin.UserIDs, b.admin.RoleIDs, b.admin.RoleNames)

	for _, uid := range b.admin.UserIDs {
		if uid == member.User.ID {
			log.Printf("isAdmin: granted — user ID match (%s)", uid)
			return true
		}
	}

	for _, roleID := range member.Roles {
		for _, adminRoleID := range b.admin.RoleIDs {
			if roleID == adminRoleID {
				log.Printf("isAdmin: granted — role ID match (%s)", roleID)
				return true
			}
		}
		role, err := s.State.Role(guildID, roleID)
		if err != nil {
			log.Printf("isAdmin: state.Role(%s, %s) error: %v", guildID, roleID, err)
			continue
		}
		for _, name := range b.admin.RoleNames {
			if strings.EqualFold(role.Name, name) {
				log.Printf("isAdmin: granted — role name match (%s == %s)", role.Name, name)
				return true
			}
		}
	}
	log.Printf("isAdmin: denied — no match found for user %s", member.User.ID)
	return false
}

// isAdminByMessage resolves the member from a message and checks admin status.
func (b *Bot) isAdminByMessage(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if m.Member == nil {
		return false
	}
	// Discord does not always populate Member.User in MessageCreate events;
	// m.Author is always set and is the same user.
	if m.Member.User == nil {
		m.Member.User = m.Author
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

// optRawString extracts a raw string value for option types whose Value is a
// string but that aren't ApplicationCommandOptionString (e.g. Attachment, User,
// Role, Channel — all send their snowflake ID as the raw value).
func optRawString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			if s, ok := o.Value.(string); ok {
				return s
			}
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

// saveAttachment downloads a Discord attachment URL and saves it to pdfDir,
// returning the path. If a file with the same name already exists it is
// overwritten so re-uploading a corrected PDF works as expected.
func (b *Bot) saveAttachment(url, filename string) (string, error) {
	resp, err := discordHTTPClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	dest := filepath.Join(b.pdfDir, filename)
	f, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dest, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(dest)
		return "", fmt.Errorf("write %s: %w", dest, err)
	}
	return dest, nil
}
