// Package captionbot is the Discord side of the captions bot: its commands
// (/transcribe start and stop, /captions-setup, Fix caption), the
// announcements around a captioned call, opting out, and leaving cleanly.
package captionbot

import (
	"sync"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/handler"
	"github.com/disgoorg/disgo/handler/middleware"
	"github.com/disgoorg/omit"
	"github.com/disgoorg/snowflake/v2"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"livetranscribebot/captions"
	"livetranscribebot/sessions"
	"livetranscribebot/settings"
	"livetranscribebot/speech"
)

// Bot holds everything the command handlers share. Handlers are its methods:
// disgo fixes a handler's arguments, so this is how they reach the settings,
// the running sessions and the speech model.
type Bot struct {
	settings   *settings.Store
	sessions   *sessions.Store
	recogniser *sherpa.OnlineRecognizer

	// posters counts captions threads still being posted to, so shutdown can
	// wait for the last captions to reach Discord.
	posters sync.WaitGroup
}

// New makes a Bot with no sessions running. recogniser is the speech model,
// loaded once and shared by every call.
func New(serverSettings *settings.Store, recogniser *sherpa.OnlineRecognizer) *Bot {
	return &Bot{
		settings:   serverSettings,
		sessions:   sessions.New(),
		recogniser: recogniser,
	}
}

// Commands are the bot's commands, registered with Discord on every start.
var Commands = []discord.ApplicationCommandCreate{
	discord.SlashCommandCreate{
		Name:        "transcribe",
		Description: "Used to start bot transcribing sessions for meetings etc",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionSubCommand{
				Name:        "start",
				Description: "Start transcribing a meeting live into a channel",
				Options: []discord.ApplicationCommandOption{
					discord.ApplicationCommandOptionString{
						Name:        "title",
						Description: "Used for the thread and channel status when the meeting begins",
						Required:    true,
					},
				},
			},
			discord.ApplicationCommandOptionSubCommand{
				Name:        "stop",
				Description: "Tells the currently active transcriber bot to wrap up notes and leave the vc",
			},
		},
	},
	// Admin-only setup. It's its own top-level command because Discord can only
	// hide whole commands: hiding /transcribe from non-admins would also hide
	// /transcribe start and stop from note-takers, who need them.
	discord.SlashCommandCreate{
		Name:        "captions-setup",
		Description: "Choose the note-taker role, captions channel and no-captions role for this server",
		// Only members with Manage Server see it by default. Admins can still
		// change who sees it in Server Settings → Integrations.
		DefaultMemberPermissions: omit.NewPtr(discord.PermissionManageGuild),
		// Servers only: it makes no sense in a DM.
		Contexts: []discord.InteractionContextType{discord.InteractionContextTypeGuild},
		Options: []discord.ApplicationCommandOption{
			// Both optional: set either or both, or neither to see the current
			// settings.
			discord.ApplicationCommandOptionRole{
				Name:        "notetaker-role",
				Description: "Members with this role can start and stop captions",
			},
			discord.ApplicationCommandOptionChannel{
				Name:        "captions-channel",
				Description: "Each call's captions thread is created in this channel",
				// Text channels only: threads can't be made in voice channels.
				ChannelTypes: []discord.ChannelType{discord.ChannelTypeGuildText},
			},
			discord.ApplicationCommandOptionRole{
				Name:        "no-captions-role",
				Description: "Members with this role are never captioned",
			},
		},
	},
	// Right-click a caption → Apps → Fix caption.
	fixCaptionCommand,
}

// Router sends each command and popup to its handler.
func (b *Bot) Router() *handler.Mux {
	r := handler.New()
	// Each interaction runs in its own goroutine, so a slow one (joining voice
	// can take seconds) doesn't hold up every other event.
	r.Use(middleware.Go)
	r.Route("/transcribe", func(r handler.Router) {
		r.Command("/start", b.onTranscribeStart)
		r.Command("/stop", b.onTranscribeStop)
	})
	r.Command("/captions-setup", b.onCaptionsSetup)
	r.MessageCommand("/Fix caption", b.onFixCaption)
	// The popup's custom ID carries which caption it's fixing and when that
	// was last edited (see fixCaptionPopup).
	r.Modal("/fix-caption/{channel}/{message}/{edited}", b.onFixCaptionSubmit)
	return r
}

// captionOutputs is where one call's captions go: posted and growing in its
// captions thread, and logged to the terminal once finished, under each
// speaker's name in this server.
func (b *Bot) captionOutputs(client *bot.Client, serverID, threadID snowflake.ID) speech.Output {
	names := captions.MemberNames(client.Caches, serverID)
	return speech.MultiOutput{
		captions.TerminalOutput{NameFor: names},
		captions.NewThreadPoster(client.Rest, threadID, names, &b.posters),
	}
}
