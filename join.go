package fileresolver

import (
	"context"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// joinLinks joins every channel or invite in links. It returns how many were resolved (joined,
// requested, or already joined) and a note for each one that failed. Only a cancelled context is
// returned as an error.
func (e *Engine) joinLinks(ctx context.Context, api API, links []string, say func(string)) (int, []string, time.Duration, error) {
	resolvedCount := 0
	var failures []string
	var limit time.Duration
	for _, link := range links {
		kind, value := joinTarget(link)
		if kind == joinNone {
			continue
		}
		name := "@" + value
		if kind == joinInvite {
			name = "https://t.me/+" + value
		}
		say("Joining " + name)
		for attempt := range 2 {
			err := e.joinOne(ctx, api, kind, value)
			if err == nil {
				resolvedCount++
				say("Joined " + name)
				e.speedUp()
				if err := e.cfg.Sleep(ctx, e.nextPause()); err != nil {
					return resolvedCount, failures, limit, err
				}
				break
			}
			if ctx.Err() != nil {
				return resolvedCount, failures, limit, ctx.Err()
			}
			if wait, limited := tgerr.AsFloodWait(err); limited {
				e.slowDown(wait)
				if attempt == 1 || wait > 180*time.Second {
					limit = max(limit, wait)
					failures = append(failures, name+" (FLOOD_WAIT "+wait.String()+")")
					say("Could not join " + name)
					break
				}
				say("Rate limit: waiting " + wait.String() + " before retrying " + name)
				if err := e.floodSleep(ctx, wait); err != nil {
					return resolvedCount, failures, limit, err
				}
				continue
			}
			e.cfg.Log.Debug("fileresolver could not join", "link", link, "err", err)
			failures = append(failures, name+" ("+describe(err)+")")
			say("Could not join " + name + ": " + describe(err))
			break
		}
	}
	return resolvedCount, failures, limit, nil
}

// joinOne joins one target. A nil error means the account is now a member, was one already, or has
// a join request pending that the bot approves by itself.
func (e *Engine) joinOne(ctx context.Context, api API, kind joinKind, value string) error {
	if kind == joinInvite {
		return e.joinInvite(ctx, api, value)
	}
	return e.joinPublic(ctx, api, value)
}

func (e *Engine) joinInvite(ctx context.Context, api API, hash string) error {
	res, err := api.MessagesImportChatInvite(ctx, hash)
	switch {
	case tgerr.Is(err, "USER_ALREADY_PARTICIPANT"):
		return nil
	case tgerr.Is(err, "INVITE_REQUEST_SENT"):
		return e.trackInvite(ctx, api, hash)
	case err != nil:
		return err
	}
	upd, err := updatesOf(res)
	if err != nil {
		return err
	}
	for _, c := range chatsOf(upd) {
		if entry, ok := entryOf(c, e.cfg.Now()); ok {
			return e.tr.recordJoin(entry)
		}
	}
	return e.trackInvite(ctx, api, hash)
}

// trackInvite follows a join-request link. Many force-subscribe bots approve the request a moment
// later, so the invite is polled until it reports a membership. If it never does, the hash is kept
// and the sweep resolves it later.
func (e *Engine) trackInvite(ctx context.Context, api API, hash string) error {
	for range 4 {
		if err := e.cfg.Sleep(ctx, 1500*time.Millisecond); err != nil {
			return err
		}
		info, err := api.MessagesCheckChatInvite(ctx, hash)
		if err != nil {
			break
		}
		if chat := memberChat(info); chat != nil {
			if entry, ok := entryOf(chat, e.cfg.Now()); ok {
				return e.tr.recordJoin(entry)
			}
		}
	}
	return e.tr.recordJoin(joinedEntry{Kind: kindInvite, Invite: hash, At: e.cfg.Now()})
}

func (e *Engine) joinPublic(ctx context.Context, api API, name string) error {
	var res *tg.ContactsResolvedPeer
	err := e.retry(ctx, func() (err error) {
		res, err = api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: name})
		return err
	})
	if err != nil {
		return err
	}
	pc, ok := res.Peer.(*tg.PeerChannel)
	if !ok {
		return errNotChannel
	}
	for _, c := range res.Chats {
		ch, ok := c.(*tg.Channel)
		if !ok || ch.ID != pc.ChannelID {
			continue
		}
		// The join call is always made, so a wrong guess here cannot stop a flow. Only the tracking
		// depends on the guess: an account that was already inside (full data, Left=false) must
		// not be tracked, or the sweep would make it leave a channel the person joined themselves.
		// Verify against real Telegram that a non-member reports Left=true.
		alreadyInside := !ch.Left && !ch.Min
		_, err := api.ChannelsJoinChannel(ctx, &tg.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash})
		if tgerr.Is(err, "USER_ALREADY_PARTICIPANT") {
			return nil
		}
		if err != nil {
			return err
		}
		if alreadyInside {
			return nil
		}
		return e.tr.recordJoin(joinedEntry{Kind: kindChannel, ID: ch.ID, AccessHash: ch.AccessHash, At: e.cfg.Now()})
	}
	return errNotChannel
}
