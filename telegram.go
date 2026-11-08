package fileresolver

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// resolved is a bot with the peer needed to write to it.
type resolved struct {
	peer     *tg.InputPeerUser
	username string
}

// resolveBot finds a bot by its public name.
func resolveBot(ctx context.Context, api API, name string) (resolved, error) {
	res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: name})
	if err != nil {
		return resolved{}, fmt.Errorf("find @%s: %w", name, err)
	}
	pu, ok := res.Peer.(*tg.PeerUser)
	if !ok {
		return resolved{}, fmt.Errorf("@%s is not a bot", name)
	}
	for _, u := range res.Users {
		if user, ok := u.(*tg.User); ok && user.ID == pu.UserID {
			return resolved{peer: &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}, username: name}, nil
		}
	}
	return resolved{}, fmt.Errorf("@%s was not returned by Telegram", name)
}

// send writes text to a bot and returns the id of the sent message. A bot the account blocked
// earlier, for instance in Cleanup, is unblocked first.
func send(ctx context.Context, api API, peer tg.InputPeerClass, text string) (int, error) {
	id, err := sendOnce(ctx, api, peer, text)
	if tgerr.Is(err, "USER_IS_BLOCKED") {
		if _, uerr := api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peer}); uerr != nil {
			return 0, uerr
		}
		return sendOnce(ctx, api, peer, text)
	}
	return id, err
}

func sendOnce(ctx context.Context, api API, peer tg.InputPeerClass, text string) (int, error) {
	rid, err := randomID()
	if err != nil {
		return 0, err
	}
	upd, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peer, Message: text, RandomID: rid})
	if err != nil {
		return 0, err
	}
	id := sentID(upd)
	if id == 0 {
		return 0, errors.New("Telegram did not report the id of the sent message")
	}
	return id, nil
}

func randomID() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil
}

func sentID(u tg.UpdatesClass) int {
	switch v := u.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		return messageID(v.Updates)
	case *tg.UpdatesCombined:
		return messageID(v.Updates)
	}
	return 0
}

func messageID(list []tg.UpdateClass) int {
	for _, up := range list {
		if m, ok := up.(*tg.UpdateMessageID); ok {
			return m.ID
		}
	}
	return 0
}

// history returns the messages a peer sent after the given id, oldest first. Our own messages are left out.
func history(ctx context.Context, api API, peer tg.InputPeerClass, after int) ([]*tg.Message, error) {
	res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 50, MinID: after})
	if err != nil {
		return nil, err
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil, nil
	}
	var out []*tg.Message
	for _, raw := range mod.GetMessages() {
		if m, ok := raw.(*tg.Message); ok && m.ID > after && !m.Out {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// updatesOf unwraps a join result to its updates.
func updatesOf(r tg.MessagesChatInviteJoinResultClass) (tg.UpdatesClass, error) {
	switch v := r.(type) {
	case *tg.MessagesChatInviteJoinResultOk:
		return v.Updates, nil
	case nil:
		return nil, nil
	}
	return nil, errors.New("Telegram asks for a web verification before this chat can be joined")
}

// chatsOf lists the chats mentioned by an updates container.
func chatsOf(u tg.UpdatesClass) []tg.ChatClass {
	switch v := u.(type) {
	case *tg.Updates:
		return v.Chats
	case *tg.UpdatesCombined:
		return v.Chats
	}
	return nil
}

// describe makes an error short enough for a progress line.
func describe(err error) string {
	if rpc, ok := tgerr.As(err); ok {
		return rpc.Type
	}
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return strings.TrimSpace(msg)
}
