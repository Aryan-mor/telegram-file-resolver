package fileresolver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	kindChannel = "channel"
	kindChat    = "chat"
	kindInvite  = "invite"
)

// joinedEntry is one chat this program joined. The access hash is kept because leaving a channel
// needs it, and the account may no longer have the channel cached.
type joinedEntry struct {
	Kind       string    `json:"kind"`
	ID         int64     `json:"id,omitempty"`
	AccessHash int64     `json:"access_hash,omitempty"`
	Invite     string    `json:"invite,omitempty"`
	At         time.Time `json:"at"`
	// Owner is the Telegram user id of the account that joined. Access hashes belong to one
	// account, so another account must not try to leave this chat and then forget it.
	Owner int64 `json:"owner,omitempty"`
}

func (j joinedEntry) key() string {
	if j.Kind == kindInvite {
		return "i:" + j.Invite
	}
	return j.Kind[:1] + ":" + strconv.FormatInt(j.ID, 10)
}

// botEntry is a bot this program talked to, kept so Cleanup can delete the chat and block it.
type botEntry struct {
	ID         int64     `json:"id"`
	AccessHash int64     `json:"access_hash"`
	Username   string    `json:"username,omitempty"`
	At         time.Time `json:"at"`
}

// CleanupResult is the outcome of Engine.Cleanup.
type CleanupResult struct {
	ChannelsLeft     int
	ChannelsFailed   int
	ChannelsStale    int
	BotsCleaned      int
	BotsFailed       int
	RateLimited      bool
	ChannelsLeftOver int
	BotsLeftOver     int
}

// tracker remembers what was joined, on disk when a directory is set.
type tracker struct {
	mu     sync.Mutex
	dir    string
	owner  int64
	joined map[string]joinedEntry
	bots   map[string]botEntry
}

func newTracker(dir string, owner int64) (*tracker, error) {
	t := &tracker{dir: dir, owner: owner, joined: map[string]joinedEntry{}, bots: map[string]botEntry{}}
	if dir == "" {
		return t, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "joined.json"), &t.joined); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "bots.json"), &t.bots); err != nil {
		return nil, err
	}
	return t, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (t *tracker) saveJoinedLocked() error {
	if t.dir == "" {
		return nil
	}
	return writeJSON(filepath.Join(t.dir, "joined.json"), t.joined)
}

func (t *tracker) saveBotsLocked() error {
	if t.dir == "" {
		return nil
	}
	return writeJSON(filepath.Join(t.dir, "bots.json"), t.bots)
}

func (t *tracker) recordJoin(e joinedEntry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.Owner = t.owner
	t.joined[e.key()] = e
	return t.saveJoinedLocked()
}

func (t *tracker) recordBot(e botEntry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bots[strconv.FormatInt(e.ID, 10)] = e
	return t.saveBotsLocked()
}

func (t *tracker) forget(key string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.joined, key)
	return t.saveJoinedLocked()
}

func (t *tracker) forgetBot(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.bots, id)
	return t.saveBotsLocked()
}

// Counts reports how many channels and bots are tracked.
func (t *tracker) counts() (channels, bots int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.joined), len(t.bots)
}

// due lists the keys to leave: everything older than leaveAfter, and the older half of the rest
// when more than max remain.
func (t *tracker) due(now time.Time, leaveAfter time.Duration, max int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var expired, active []string
	for k, e := range t.joined {
		if !t.mineLocked(e) {
			continue
		}
		if now.Sub(e.At) >= leaveAfter {
			expired = append(expired, k)
		} else {
			active = append(active, k)
		}
	}
	sort.Slice(expired, func(i, j int) bool { return t.joined[expired[i]].At.Before(t.joined[expired[j]].At) })
	sort.Slice(active, func(i, j int) bool { return t.joined[active[i]].At.Before(t.joined[active[j]].At) })
	if len(active) > max {
		expired = append(expired, active[:len(active)/2]...)
	}
	return expired
}

func (t *tracker) entry(key string) (joinedEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.joined[key]
	return e, ok
}

func (t *tracker) allKeys() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.joined))
	for k, e := range t.joined {
		if t.mineLocked(e) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return t.joined[keys[i]].At.Before(t.joined[keys[j]].At) })
	return keys
}

// leave leaves one tracked chat. It returns false when the account is no longer a member or the
// chat cannot be found, so the entry is only stale.
func (t *tracker) leave(ctx context.Context, api API, e joinedEntry) (bool, error) {
	switch e.Kind {
	case kindChannel:
		_, err := api.ChannelsLeaveChannel(ctx, &tg.InputChannel{ChannelID: e.ID, AccessHash: e.AccessHash})
		return notStale(err)
	case kindChat:
		_, err := api.MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{ChatID: e.ID, UserID: &tg.InputUserSelf{}})
		return notStale(err)
	case kindInvite:
		info, err := api.MessagesCheckChatInvite(ctx, e.Invite)
		if err != nil {
			return notStale(err)
		}
		chat := memberChat(info)
		if chat == nil {
			return false, nil
		}
		entry, ok := entryOf(chat, e.At)
		if !ok || entry.Kind == kindInvite {
			return false, nil
		}
		return t.leave(ctx, api, entry)
	}
	return false, nil
}

// notStale turns "not a member" style errors into a stale result and keeps every other error.
func notStale(err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if tgerr.Is(err, "USER_NOT_PARTICIPANT", "CHANNEL_PRIVATE", "CHANNEL_INVALID", "CHAT_ID_INVALID", "PEER_ID_INVALID", "INVITE_HASH_EXPIRED", "INVITE_HASH_INVALID") {
		return false, nil
	}
	return false, err
}

func memberChat(info tg.ChatInviteClass) tg.ChatClass {
	switch v := info.(type) {
	case *tg.ChatInviteAlready:
		return v.Chat
	case *tg.ChatInvitePeek:
		return v.Chat
	}
	return nil
}

func entryOf(c tg.ChatClass, at time.Time) (joinedEntry, bool) {
	switch v := c.(type) {
	case *tg.Channel:
		return joinedEntry{Kind: kindChannel, ID: v.ID, AccessHash: v.AccessHash, At: at}, true
	case *tg.Chat:
		return joinedEntry{Kind: kindChat, ID: v.ID, At: at}, true
	}
	return joinedEntry{}, false
}

// sweep leaves what is due. A rate limit stops it and is returned, so the caller can wait; the
// untouched entries stay tracked.
func (t *tracker) sweep(ctx context.Context, api API, cfg Config) error {
	for _, key := range t.due(cfg.Now(), cfg.LeaveAfter, cfg.MaxJoined) {
		e, ok := t.entry(key)
		if !ok {
			continue
		}
		_, err := t.leave(ctx, api, e)
		if _, limited := tgerr.AsFloodWait(err); limited {
			return err
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			cfg.Log.Warn("fileresolver could not leave a chat", "key", key, "err", err)
		}
		if err := t.forget(key); err != nil {
			return err
		}
		if err := cfg.Sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
	return nil
}

// cleanup leaves every tracked chat, then deletes and blocks every tracked bot.
func (t *tracker) cleanup(ctx context.Context, api API, cfg Config) (CleanupResult, error) {
	var res CleanupResult
	keys := t.allKeys()
	for i, key := range keys {
		e, ok := t.entry(key)
		if !ok {
			continue
		}
		left, err := t.leave(ctx, api, e)
		if _, limited := tgerr.AsFloodWait(err); limited {
			res.RateLimited = true
			res.ChannelsLeftOver = len(keys) - i
			_, res.BotsLeftOver = t.counts()
			return res, nil
		}
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.ChannelsFailed++
		case left:
			res.ChannelsLeft++
		default:
			res.ChannelsStale++
		}
		if err := t.forget(key); err != nil {
			return res, err
		}
		if err := cfg.Sleep(ctx, 1500*time.Millisecond); err != nil {
			return res, err
		}
	}

	t.mu.Lock()
	ids := make([]string, 0, len(t.bots))
	for id := range t.bots {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	sort.Strings(ids)
	for i, id := range ids {
		t.mu.Lock()
		b := t.bots[id]
		t.mu.Unlock()
		peer := &tg.InputPeerUser{UserID: b.ID, AccessHash: b.AccessHash}
		err := wipeBot(ctx, api, peer)
		if _, limited := tgerr.AsFloodWait(err); limited {
			res.RateLimited = true
			res.BotsLeftOver = len(ids) - i
			return res, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			res.BotsFailed++
		} else {
			res.BotsCleaned++
		}
		if err := t.forgetBot(id); err != nil {
			return res, err
		}
		if err := cfg.Sleep(ctx, 1500*time.Millisecond); err != nil {
			return res, err
		}
	}
	return res, nil
}

// wipeBot deletes the whole chat with a bot for both sides, then blocks it.
func wipeBot(ctx context.Context, api API, peer tg.InputPeerClass) error {
	for range 50 {
		res, err := api.MessagesDeleteHistory(ctx, &tg.MessagesDeleteHistoryRequest{Peer: peer, Revoke: true})
		if err != nil {
			return err
		}
		if res == nil || res.Offset == 0 {
			break
		}
	}
	_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peer})
	return err
}

// botPeers rebuilds the bot lookup from what an earlier run saved, so a restart does not resolve
// the same names again.
func (t *tracker) botPeers() map[string]*tg.InputPeerUser {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]*tg.InputPeerUser{}
	for _, b := range t.bots {
		if b.Username != "" {
			out[b.Username] = &tg.InputPeerUser{UserID: b.ID, AccessHash: b.AccessHash}
		}
	}
	return out
}

// mineLocked is true for entries this account may leave: its own, and old ones with no owner.
func (t *tracker) mineLocked(e joinedEntry) bool {
	return t.owner == 0 || e.Owner == 0 || e.Owner == t.owner
}

// bind sets the account whose chats the next calls may leave. An API that reports its account
// through UserID() wins over Config.Owner, so a host with several accounts stays correct.
func (t *tracker) bind(api API, fallback int64) {
	owner := fallback
	if o, ok := api.(interface{ UserID() int64 }); ok && o.UserID() != 0 {
		owner = o.UserID()
	}
	t.mu.Lock()
	t.owner = owner
	t.mu.Unlock()
}
