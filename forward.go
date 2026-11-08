package fileresolver

import (
	"context"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Sink receives what the bot finally sent. A host that wants the messages itself reads
// Result.Messages instead and leaves Options.Sink empty.
type Sink interface {
	// Deliver passes msgs, which live in the chat from, on. A bot may delete its messages after a
	// few minutes and Cleanup deletes the whole chat, so the copies are what a host should keep.
	Deliver(ctx context.Context, api API, from tg.InputPeerClass, msgs []*tg.Message) (Delivery, error)
}

// Delivery says what a Sink passed on.
type Delivery struct {
	// Count is how many messages arrived.
	Count int
	// Messages are the new copies, when Telegram reported them. They may be fewer than Count.
	Messages []*tg.Message
	// Peer is the chat the copies live in.
	Peer tg.InputPeerClass
}

// ForwardSink forwards the messages to a chat. A nil To means Saved Messages.
type ForwardSink struct {
	To    tg.InputPeerClass
	Chunk int
	Delay time.Duration
	// Sleep replaces the real wait, for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Deliver forwards in chunks, waits out rate limits and skips chunks the chat forbids copying.
func (f ForwardSink) Deliver(ctx context.Context, api API, from tg.InputPeerClass, msgs []*tg.Message) (Delivery, error) {
	to := f.To
	if to == nil {
		to = &tg.InputPeerSelf{}
	}
	chunk, delay := f.Chunk, f.Delay
	if chunk <= 0 {
		chunk = 5
	}
	if delay <= 0 {
		delay = 1500 * time.Millisecond
	}
	nap := f.Sleep
	if nap == nil {
		nap = sleep
	}
	out := Delivery{Peer: to}
	for i := 0; i < len(msgs); i += chunk {
		part := msgs[i:min(i+chunk, len(msgs))]
		ids := make([]int, len(part))
		rids := make([]int64, len(part))
		for j, m := range part {
			ids[j] = m.ID
			r, err := randomID()
			if err != nil {
				return out, err
			}
			rids[j] = r
		}
		for attempt := range 3 {
			upd, err := api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
				FromPeer: from, ToPeer: to, ID: ids, RandomID: rids,
			})
			if err == nil {
				out.Count += len(part)
				out.Messages = append(out.Messages, newMessages(upd)...)
				break
			}
			if wait, limited := tgerr.AsFloodWait(err); limited && attempt < 2 {
				if err := nap(ctx, wait+time.Second); err != nil {
					return out, err
				}
				continue
			}
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			// A restricted chat or any other RPC error skips this chunk only.
			break
		}
		if i+chunk < len(msgs) {
			if err := nap(ctx, delay); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// newMessages reads the messages a send or forward created out of its updates.
func newMessages(u tg.UpdatesClass) []*tg.Message {
	var list []tg.UpdateClass
	switch v := u.(type) {
	case *tg.Updates:
		list = v.Updates
	case *tg.UpdatesCombined:
		list = v.Updates
	}
	var out []*tg.Message
	for _, up := range list {
		switch v := up.(type) {
		case *tg.UpdateNewMessage:
			if m, ok := v.Message.(*tg.Message); ok {
				out = append(out, m)
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := v.Message.(*tg.Message); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
