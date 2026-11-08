// Package fileresolver follows a Telegram bot deep link, joins the channels the bot asks for, collects
// what the bot finally sends, and leaves the channels again later.
//
// It runs on a Telegram user account through gotd. It can own that account itself (see
// cmd/telegram-file-resolver) or borrow one that a host program already keeps connected, through Provider.
// Submit is a plain function call, so a host never has to write a message into a chat to start work.
//
// The behaviour follows the Python userbot https://github.com/AmirHBuilds/fuckjoin (MIT), which
// this module was written to replace. No code is copied from it.
package fileresolver

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/gotd/td/tg"
)

// API is the part of the Telegram client that telegram-file-resolver calls. *tg.Client satisfies it, and a test
// can replace it.
type API interface {
	ContactsResolveUsername(ctx context.Context, req *tg.ContactsResolveUsernameRequest) (*tg.ContactsResolvedPeer, error)
	ContactsBlock(ctx context.Context, req *tg.ContactsBlockRequest) (bool, error)
	ContactsUnblock(ctx context.Context, req *tg.ContactsUnblockRequest) (bool, error)
	MessagesSendMessage(ctx context.Context, req *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error)
	MessagesGetHistory(ctx context.Context, req *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error)
	MessagesImportChatInvite(ctx context.Context, hash string) (tg.MessagesChatInviteJoinResultClass, error)
	MessagesCheckChatInvite(ctx context.Context, hash string) (tg.ChatInviteClass, error)
	MessagesForwardMessages(ctx context.Context, req *tg.MessagesForwardMessagesRequest) (tg.UpdatesClass, error)
	MessagesDeleteHistory(ctx context.Context, req *tg.MessagesDeleteHistoryRequest) (*tg.MessagesAffectedHistory, error)
	MessagesDeleteChatUser(ctx context.Context, req *tg.MessagesDeleteChatUserRequest) (tg.UpdatesClass, error)
	ChannelsJoinChannel(ctx context.Context, channel tg.InputChannelClass) (tg.MessagesChatInviteJoinResultClass, error)
	ChannelsLeaveChannel(ctx context.Context, channel tg.InputChannelClass) (tg.UpdatesClass, error)
}

var _ API = (*tg.Client)(nil)

// Provider lends a connected Telegram account for the length of fn. The API it hands out may also
// have a UserID() int64 method; the engine then stamps and filters tracked chats with that id. A host that already keeps a
// session open implements it by calling fn with that session's client. Run must return fn's error.
type Provider interface {
	Run(ctx context.Context, fn func(ctx context.Context, api API) error) error
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context, fn func(ctx context.Context, api API) error) error

// Run calls f.
func (f ProviderFunc) Run(ctx context.Context, fn func(ctx context.Context, api API) error) error {
	return f(ctx, fn)
}

// Config holds the pace and the filters. DefaultConfig gives the values that suit most accounts.
type Config struct {
	// StateDir keeps the list of channels and bots this program touched, so a restart still
	// leaves them later. Empty keeps the list in memory only.
	StateDir string
	// Owner is the Telegram user id of the account the Provider lends, when it is known. Entries
	// are stamped with it, and an engine only leaves chats stamped with its own id. Use one engine
	// and one StateDir per account when a host keeps several.
	Owner int64
	// RetryDelay waits after a channel was joined before /start is sent again.
	RetryDelay time.Duration
	// PacingDelay is the usual wait after each join, so Telegram does not rate-limit the account.
	// The real wait is spread randomly around the current pace (half to one and a half times it).
	PacingDelay time.Duration
	// PacingMax caps the pace. After a FLOOD_WAIT the pace doubles up to this value, and every
	// successful join brings it back toward PacingDelay. Default 3 minutes.
	PacingMax time.Duration
	// Rand returns a number in [0,1) for the spread. Nil uses math/rand.
	// LeaveAfter is how long a joined channel stays before the sweep leaves it.
	LeaveAfter time.Duration
	// MaxJoined is the number of tracked channels above which the oldest half is left.
	MaxJoined int
	// TransientTexts are spinner messages a bot sends before its real answer.
	TransientTexts []string
	// BlockedWords drop a text-only message that contains one of them. Messages with media are kept.
	BlockedWords []string
	// ResponseTimeout is how long a bot may take to send an actionable message.
	ResponseTimeout time.Duration
	// FollowupWindow is how long the answer must stay unchanged before it counts as complete.
	FollowupWindow time.Duration
	// PollInterval is how often the chat with a bot is read.
	PollInterval time.Duration
	// SweepEvery is how often Run looks for channels to leave.
	SweepEvery time.Duration
	// MaxStartAttempts bounds the join and re-send cycle for one bot.
	MaxStartAttempts int
	// MaxHandoffs bounds how many bots one link may lead through.
	MaxHandoffs int
	// Log receives progress at debug level and problems at warn level. Nil discards.
	Log  *slog.Logger
	Rand func() float64
	// Sleep and Now exist for tests.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
}

// DefaultConfig returns the pace the Python userbot used.
func DefaultConfig() Config {
	return Config{
		RetryDelay:       2 * time.Second,
		PacingDelay:      3 * time.Second,
		LeaveAfter:       24 * time.Hour,
		MaxJoined:        100,
		TransientTexts:   []string{"...", "please wait", "processing...", "⏳", "⌛"},
		ResponseTimeout:  30 * time.Second,
		FollowupWindow:   2 * time.Second,
		PollInterval:     time.Second,
		SweepEvery:       time.Minute,
		MaxStartAttempts: 5,
		MaxHandoffs:      5,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.LeaveAfter <= 0 {
		c.LeaveAfter = d.LeaveAfter
	}
	if c.MaxJoined <= 0 {
		c.MaxJoined = d.MaxJoined
	}
	if c.ResponseTimeout <= 0 {
		c.ResponseTimeout = d.ResponseTimeout
	}
	if c.FollowupWindow <= 0 {
		c.FollowupWindow = d.FollowupWindow
	}
	if c.PollInterval <= 0 {
		c.PollInterval = d.PollInterval
	}
	if c.SweepEvery <= 0 {
		c.SweepEvery = d.SweepEvery
	}
	if c.MaxStartAttempts <= 0 {
		c.MaxStartAttempts = d.MaxStartAttempts
	}
	if c.MaxHandoffs <= 0 {
		c.MaxHandoffs = d.MaxHandoffs
	}
	if c.PacingMax <= 0 {
		c.PacingMax = 3 * time.Minute
	}
	if c.PacingMax < c.PacingDelay {
		c.PacingMax = c.PacingDelay
	}
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
	if c.Sleep == nil {
		c.Sleep = sleep
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	return c
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
