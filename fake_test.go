package fileresolver

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// fakeClock moves only when something sleeps, so tests run at once.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

// fake is a scripted Telegram: bots answer through script, channels can be joined and left.
type fake struct {
	mu          sync.Mutex
	clock       *fakeClock
	nextID      int
	users       map[string]*tg.User
	channels    map[string]*tg.Channel // public name -> channel
	invites     map[string]*tg.Channel // invite hash -> channel
	pending     map[string]*tg.Channel // join-request hash -> channel, approved on first check
	hist        map[int64][]*tg.Message
	sent        []string
	joined      []int64
	left        []int64
	forwards    [][]int
	blocked     map[int64]bool
	flood       int // forward calls that fail with FLOOD_WAIT before one succeeds
	resolves    int
	inviteFlood time.Duration // ImportChatInvite fails with this FLOOD_WAIT while it is set

	script    func(f *fake, bot int64, text string)
	onHistory func(f *fake, bot int64)
}

func newFake() *fake {
	return &fake{
		clock:    &fakeClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)},
		nextID:   1,
		users:    map[string]*tg.User{},
		channels: map[string]*tg.Channel{},
		invites:  map[string]*tg.Channel{},
		pending:  map[string]*tg.Channel{},
		hist:     map[int64][]*tg.Message{},
		blocked:  map[int64]bool{},
	}
}

func (f *fake) config() Config {
	c := DefaultConfig()
	c.Now, c.Sleep = f.clock.Now, f.clock.Sleep
	return c
}

func (f *fake) provider() Provider {
	return ProviderFunc(func(ctx context.Context, fn func(context.Context, API) error) error { return fn(ctx, f) })
}

func (f *fake) addBot(name string, id int64) {
	f.users[name] = &tg.User{ID: id, AccessHash: id * 7, Bot: true, Username: name}
}

// reply posts a message from a bot into the chat and returns it.
func (f *fake) reply(bot int64, m *tg.Message) *tg.Message {
	m.ID = f.nextID
	f.nextID++
	f.hist[bot] = append(f.hist[bot], m)
	return m
}

func video(caption string) *tg.Message {
	return &tg.Message{Message: caption, Media: &tg.MessageMediaDocument{}}
}

func linkButton(url string) tg.ReplyMarkupClass {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{{Buttons: []tg.KeyboardInlineButton{{Text: "join", Type: &tg.InlineButtonTypeURL{URL: url}}}}}}
}

func (f *fake) ContactsResolveUsername(_ context.Context, req *tg.ContactsResolveUsernameRequest) (*tg.ContactsResolvedPeer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if u, ok := f.users[req.Username]; ok {
		return &tg.ContactsResolvedPeer{Peer: &tg.PeerUser{UserID: u.ID}, Users: []tg.UserClass{u}}, nil
	}
	if ch, ok := f.channels[req.Username]; ok {
		return &tg.ContactsResolvedPeer{Peer: &tg.PeerChannel{ChannelID: ch.ID}, Chats: []tg.ChatClass{ch}}, nil
	}
	return nil, tgerr.New(400, "USERNAME_NOT_OCCUPIED")
}

func (f *fake) ContactsBlock(_ context.Context, req *tg.ContactsBlockRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[req.ID.(*tg.InputPeerUser).UserID] = true
	return true, nil
}

func (f *fake) ContactsUnblock(_ context.Context, req *tg.ContactsUnblockRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.blocked, req.ID.(*tg.InputPeerUser).UserID)
	return true, nil
}

func (f *fake) MessagesSendMessage(_ context.Context, req *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bot := req.Peer.(*tg.InputPeerUser).UserID
	if f.blocked[bot] {
		return nil, tgerr.New(403, "USER_IS_BLOCKED")
	}
	id := f.nextID
	f.nextID++
	f.sent = append(f.sent, req.Message)
	if f.script != nil {
		f.script(f, bot, req.Message)
	}
	return &tg.UpdateShortSentMessage{ID: id}, nil
}

func (f *fake) MessagesGetHistory(_ context.Context, req *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bot := req.Peer.(*tg.InputPeerUser).UserID
	if f.onHistory != nil {
		f.onHistory(f, bot)
	}
	var out []tg.MessageClass
	for _, m := range f.hist[bot] {
		if m.ID > req.MinID {
			cp := *m
			out = append(out, &cp)
		}
	}
	return &tg.MessagesMessages{Messages: out}, nil
}

func (f *fake) MessagesImportChatInvite(_ context.Context, hash string) (tg.MessagesChatInviteJoinResultClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inviteFlood > 0 {
		return nil, tgerr.New(420, "FLOOD_WAIT_"+strconv.Itoa(int(f.inviteFlood.Seconds())))
	}
	if ch, ok := f.pending[hash]; ok {
		_ = ch
		return nil, tgerr.New(400, "INVITE_REQUEST_SENT")
	}
	ch, ok := f.invites[hash]
	if !ok {
		return nil, tgerr.New(400, "INVITE_HASH_INVALID")
	}
	if !ch.Left {
		return nil, tgerr.New(400, "USER_ALREADY_PARTICIPANT")
	}
	ch.Left = false
	f.joined = append(f.joined, ch.ID)
	return &tg.MessagesChatInviteJoinResultOk{Updates: &tg.Updates{Chats: []tg.ChatClass{ch}}}, nil
}

func (f *fake) MessagesCheckChatInvite(_ context.Context, hash string) (tg.ChatInviteClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.pending[hash]; ok {
		// The bot approves the request as soon as it is looked at once.
		delete(f.pending, hash)
		f.invites[hash] = ch
		ch.Left = false
		f.joined = append(f.joined, ch.ID)
		return &tg.ChatInviteAlready{Chat: ch}, nil
	}
	if ch, ok := f.invites[hash]; ok && !ch.Left {
		return &tg.ChatInviteAlready{Chat: ch}, nil
	}
	return &tg.ChatInvite{}, nil
}

func (f *fake) MessagesForwardMessages(_ context.Context, req *tg.MessagesForwardMessagesRequest) (tg.UpdatesClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flood > 0 {
		f.flood--
		return nil, tgerr.New(420, "FLOOD_WAIT_1")
	}
	f.forwards = append(f.forwards, append([]int(nil), req.ID...))
	return &tg.Updates{}, nil
}

func (f *fake) MessagesDeleteHistory(_ context.Context, req *tg.MessagesDeleteHistoryRequest) (*tg.MessagesAffectedHistory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hist, req.Peer.(*tg.InputPeerUser).UserID)
	return &tg.MessagesAffectedHistory{}, nil
}

func (f *fake) MessagesDeleteChatUser(context.Context, *tg.MessagesDeleteChatUserRequest) (tg.UpdatesClass, error) {
	return &tg.Updates{}, nil
}

func (f *fake) ChannelsJoinChannel(_ context.Context, in tg.InputChannelClass) (tg.MessagesChatInviteJoinResultClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := in.(*tg.InputChannel).ChannelID
	for _, ch := range f.channels {
		if ch.ID == id {
			ch.Left = false
			f.joined = append(f.joined, id)
		}
	}
	return &tg.MessagesChatInviteJoinResultOk{Updates: &tg.Updates{}}, nil
}

func (f *fake) ChannelsLeaveChannel(_ context.Context, in tg.InputChannelClass) (tg.UpdatesClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.left = append(f.left, in.(*tg.InputChannel).ChannelID)
	return &tg.Updates{}, nil
}
