package fileresolver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

var (
	// ErrNoReply means the bot sent nothing actionable before ResponseTimeout.
	ErrNoReply = errors.New("the bot did not answer in time")
	// ErrNoContent means the bot answered, but nothing was left to deliver: every message was
	// protected, filtered or empty.
	ErrNoContent = errors.New("no deliverable content")

	errNotChannel = errors.New("the link is not a channel")
)

// Options tune one Submit.
type Options struct {
	// Progress receives one short line per step. Nil ignores them.
	Progress func(line string)
	// Sink gets the final messages. Nil leaves them in Result.Messages only.
	Sink Sink
}

// Result describes one finished link.
type Result struct {
	// Bot is the last bot of the chain, the one that sent Messages.
	Bot string
	// Peer is the chat with that bot. Messages live there.
	Peer tg.InputPeerClass
	// Messages are what is left after protected, filtered and empty ones are dropped.
	Messages []*tg.Message
	// Delivered is what the Sink passed on; zero when no Sink was set. Its Messages are the
	// durable copies, since the bot chat can be emptied by the bot or by Cleanup.
	Delivered Delivery
	// Joined counts channels resolved on the way, Handoffs the extra bots followed.
	Joined   int
	Handoffs int
	// Failures notes each channel that could not be joined.
	Failures []string
	// RateLimit is the longest FLOOD_WAIT Telegram asked for while joining. Zero if none.
	RateLimit time.Duration
}

// RateLimitError means joining was refused for Wait and nothing could be delivered. It is not the
// link's fault: wait, then submit the same link again.
type RateLimitError struct{ Wait time.Duration }

func (e *RateLimitError) Error() string {
	return "Telegram limits joining channels for " + e.Wait.String()
}

// Engine runs links one at a time on one account.
type Engine struct {
	p    Provider
	cfg  Config
	tr   *tracker
	gate chan struct{}

	paceMu sync.Mutex
	pace   time.Duration

	peersMu sync.Mutex
	peers   map[string]*tg.InputPeerUser
}

// New builds an engine. cfg.StateDir is created if it is set.
func New(p Provider, cfg Config) (*Engine, error) {
	cfg = cfg.withDefaults()
	tr, err := newTracker(cfg.StateDir, cfg.Owner)
	if err != nil {
		return nil, fmt.Errorf("file resolver state: %w", err)
	}
	return &Engine{p: p, cfg: cfg, tr: tr, gate: make(chan struct{}, 1), peers: tr.botPeers(), pace: cfg.PacingDelay}, nil
}

// Tracked reports how many joined channels and contacted bots wait for cleanup.
func (e *Engine) Tracked() (channels, bots int) { return e.tr.counts() }

// Submit follows one bot link and returns what the chain of bots sent. Links wait for each other,
// because two flows on one account would mix up the replies. Cancel ctx to stop a running link.
func (e *Engine) Submit(ctx context.Context, link string, opt Options) (*Result, error) {
	start, err := ParseStartLink(link)
	if err != nil {
		return nil, err
	}
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	say := func(line string) {
		e.cfg.Log.Debug("fileresolver", "step", line)
		if opt.Progress != nil {
			opt.Progress(line)
		}
	}
	var res *Result
	var runErr error
	err = e.p.Run(ctx, func(ctx context.Context, api API) error {
		e.tr.bind(api, e.cfg.Owner)
		res, runErr = e.follow(ctx, api, start, opt, say)
		return runErr
	})
	if runErr != nil {
		return res, runErr
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// Run sweeps old channels until ctx ends. Call it once beside Submit; it needs no flow to be active.
func (e *Engine) Run(ctx context.Context) error {
	for {
		if err := e.cfg.Sleep(ctx, e.cfg.SweepEvery); err != nil {
			return err
		}
		if ch, _ := e.tr.counts(); ch == 0 {
			continue
		}
		if len(e.tr.due(e.cfg.Now(), e.cfg.LeaveAfter, e.cfg.MaxJoined)) == 0 {
			continue
		}
		select {
		case e.gate <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		err := e.p.Run(ctx, func(ctx context.Context, api API) error {
			e.tr.bind(api, e.cfg.Owner)
			return e.tr.sweep(ctx, api, e.cfg)
		})
		<-e.gate
		if wait, limited := tgerr.AsFloodWait(err); limited {
			e.cfg.Log.Warn("fileresolver sweep is rate limited", "wait", wait)
			if err := e.cfg.Sleep(ctx, min(wait, time.Hour)+time.Second); err != nil {
				return err
			}
		} else if err != nil && ctx.Err() == nil {
			e.cfg.Log.Warn("fileresolver sweep failed", "err", err)
		}
	}
}

// Cleanup leaves every tracked channel now, then deletes and blocks every tracked bot.
func (e *Engine) Cleanup(ctx context.Context) (CleanupResult, error) {
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return CleanupResult{}, ctx.Err()
	}
	var res CleanupResult
	err := e.p.Run(ctx, func(ctx context.Context, api API) error {
		var err error
		e.tr.bind(api, e.cfg.Owner)
		res, err = e.tr.cleanup(ctx, api, e.cfg)
		return err
	})
	return res, err
}

func (e *Engine) follow(ctx context.Context, api API, start StartLink, opt Options, say func(string)) (*Result, error) {
	res := &Result{}
	cur := start
	seen := map[string]bool{cur.Key(): true}
	var msgs []*tg.Message
	var peer tg.InputPeerClass
	for flow := 0; flow < e.cfg.MaxHandoffs; flow++ {
		got, p, joined, failures, limit, err := e.botFlow(ctx, api, cur, say)
		res.Joined += joined
		res.RateLimit = max(res.RateLimit, limit)
		res.Failures = append(res.Failures, failures...)
		if err != nil {
			return res, err
		}
		msgs, peer = got, p
		res.Bot = cur.Bot
		next := ""
		var nextStart StartLink
		for _, l := range linksOf(msgs) {
			if s, err := ParseStartLink(l); err == nil && !seen[s.Key()] {
				nextStart, next = s, l
				break
			}
		}
		if next == "" {
			break
		}
		seen[nextStart.Key()] = true
		cur = nextStart
		res.Handoffs++
		say("Following handoff to @" + cur.Bot)
	}
	res.Peer = peer

	say(fmt.Sprintf("Fetching %d final message(s)", len(msgs)))
	for _, m := range msgs {
		switch {
		case m.Noforwards:
			say("Skipped protected message")
		case isBlocked(m, e.cfg.BlockedWords):
			say("Skipped filtered message")
		case !hasMedia(m) && strings.TrimSpace(m.Message) == "":
		default:
			res.Messages = append(res.Messages, m)
		}
	}
	// A text-only answer after a refused join is the bot repeating its join request, not content.
	if res.RateLimit > 0 && !anyMedia(res.Messages) {
		return res, &RateLimitError{Wait: res.RateLimit}
	}
	if len(res.Messages) == 0 {
		return res, noContent(res.Failures)
	}
	if opt.Sink != nil {
		d, err := opt.Sink.Deliver(ctx, api, peer, res.Messages)
		res.Delivered = d
		if err != nil {
			return res, err
		}
		if d.Count == 0 {
			return res, errors.New("the content could not be delivered")
		}
	}
	return res, nil
}

func noContent(failures []string) error {
	if len(failures) == 0 {
		return ErrNoContent
	}
	return fmt.Errorf("%w; could not join: %s", ErrNoContent, strings.Join(failures, "; "))
}

// botFlow sends /start, joins what the bot asks for and sends /start again, until the bot asks
// for nothing more. It returns the bot's final messages.
func (e *Engine) botFlow(ctx context.Context, api API, start StartLink, say func(string)) ([]*tg.Message, tg.InputPeerClass, int, []string, time.Duration, error) {
	bot, err := e.bot(ctx, api, start.Bot)
	if err != nil {
		return nil, nil, 0, nil, 0, err
	}
	if err := e.tr.recordBot(botEntry{ID: bot.peer.UserID, AccessHash: bot.peer.AccessHash, Username: bot.username, At: e.cfg.Now()}); err != nil {
		return nil, nil, 0, nil, 0, err
	}
	command := "/start " + start.Argument
	say("Sending /start to @" + start.Bot)
	msgs, err := e.ask(ctx, api, bot.peer, command)
	if err != nil {
		return nil, nil, 0, nil, 0, err
	}
	say("Received a response from @" + start.Bot)

	joined := 0
	var failures []string
	var limit time.Duration
	seen := map[string]bool{}
	for range e.cfg.MaxStartAttempts {
		var fresh []string
		for _, l := range linksOf(msgs) {
			if !seen[l] {
				seen[l] = true
				fresh = append(fresh, l)
			}
		}
		n, fails, lim, err := e.joinLinks(ctx, api, fresh, say)
		limit = max(limit, lim)
		joined += n
		failures = append(failures, fails...)
		if err != nil {
			return nil, nil, joined, failures, limit, err
		}
		if n == 0 {
			break
		}
		if err := e.cfg.Sleep(ctx, e.cfg.RetryDelay); err != nil {
			return nil, nil, joined, failures, limit, err
		}
		say("Re-sending /start to @" + start.Bot)
		if msgs, err = e.ask(ctx, api, bot.peer, command); err != nil {
			return nil, nil, joined, failures, limit, err
		}
		say("Received an updated response from @" + start.Bot)
	}
	return msgs, bot.peer, joined, failures, limit, nil
}

// ask sends text and waits for the bot's answer to settle. Wait indicators are ignored, and a
// message the bot edits is read in its latest form. The answer is complete when it has not
// changed for FollowupWindow.
func (e *Engine) ask(ctx context.Context, api API, peer tg.InputPeerClass, text string) ([]*tg.Message, error) {
	var after int
	err := e.retry(ctx, func() (err error) {
		after, err = send(ctx, api, peer, text)
		return err
	})
	if err != nil {
		return nil, err
	}
	started := e.cfg.Now()
	var last []*tg.Message
	var sig string
	var changed time.Time
	for {
		var msgs []*tg.Message
		err := e.retry(ctx, func() (err error) {
			msgs, err = history(ctx, api, peer, after)
			return err
		})
		if err != nil {
			return nil, err
		}
		var actionable []*tg.Message
		for _, m := range msgs {
			if !isTransient(m, e.cfg.TransientTexts) {
				actionable = append(actionable, m)
			}
		}
		now := e.cfg.Now()
		if len(actionable) > 0 {
			if s := signature(actionable); s != sig {
				sig, changed = s, now
			} else if now.Sub(changed) >= e.cfg.FollowupWindow {
				return actionable, nil
			}
			last = actionable
		}
		if waited := now.Sub(started); waited >= e.cfg.ResponseTimeout {
			if len(last) > 0 && waited >= 2*e.cfg.ResponseTimeout {
				return last, nil
			}
			if len(last) == 0 {
				return nil, ErrNoReply
			}
		}
		if err := e.cfg.Sleep(ctx, e.cfg.PollInterval); err != nil {
			return nil, err
		}
	}
}

func signature(msgs []*tg.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "%d:%d:%d:%t;", m.ID, m.EditDate, len(m.Message), hasMedia(m))
	}
	return b.String()
}

// retry runs fn again after a short rate limit. contacts.resolveUsername and messages.getHistory
// are limited tightly, and a flow that polls must not die on the first FLOOD_WAIT.
func (e *Engine) retry(ctx context.Context, fn func() error) error {
	var err error
	for range 3 {
		if err = fn(); err == nil {
			return nil
		}
		wait, limited := tgerr.AsFloodWait(err)
		if !limited || wait > 180*time.Second {
			return err
		}
		e.cfg.Log.Debug("gatehop rate limited", "wait", wait)
		if serr := e.cfg.Sleep(ctx, wait+time.Second); serr != nil {
			return serr
		}
	}
	return err
}

// bot finds a bot, asking Telegram only the first time: resolving names is limited tightly.
func (e *Engine) bot(ctx context.Context, api API, name string) (resolved, error) {
	e.peersMu.Lock()
	peer := e.peers[name]
	e.peersMu.Unlock()
	if peer != nil {
		return resolved{peer: peer, username: name}, nil
	}
	var r resolved
	err := e.retry(ctx, func() (err error) {
		r, err = resolveBot(ctx, api, name)
		return err
	})
	if err != nil {
		return resolved{}, err
	}
	e.peersMu.Lock()
	e.peers[name] = r.peer
	e.peersMu.Unlock()
	return r, nil
}

// Provider returns the provider the engine was built with.
func (e *Engine) Provider() Provider { return e.p }

func anyMedia(msgs []*tg.Message) bool {
	for _, m := range msgs {
		if hasMedia(m) {
			return true
		}
	}
	return false
}

// nextPause is how long to wait after a join: the current pace, spread so that joins do not land
// on a fixed beat, and never beyond PacingMax.
func (e *Engine) nextPause() time.Duration {
	e.paceMu.Lock()
	pace := e.pace
	e.paceMu.Unlock()
	if pace <= 0 {
		return 0
	}
	d := time.Duration(float64(pace) * (0.5 + e.cfg.Rand()))
	return min(d, e.cfg.PacingMax)
}

// slowDown reacts to a FLOOD_WAIT: the pace doubles, and is at least a quarter of what Telegram asked for.
func (e *Engine) slowDown(wait time.Duration) {
	e.paceMu.Lock()
	defer e.paceMu.Unlock()
	next := max(e.pace*2, wait/4, e.cfg.PacingDelay)
	e.pace = min(next, e.cfg.PacingMax)
}

// speedUp eases the pace back toward PacingDelay after a join went through.
func (e *Engine) speedUp() {
	e.paceMu.Lock()
	defer e.paceMu.Unlock()
	e.pace = max(e.cfg.PacingDelay, e.pace*3/4)
}

// floodSleep waits out a FLOOD_WAIT with a little extra, so retries do not all start on the second.
func (e *Engine) floodSleep(ctx context.Context, wait time.Duration) error {
	extra := time.Duration(float64(wait) * 0.15 * e.cfg.Rand())
	return e.cfg.Sleep(ctx, wait+time.Second+extra)
}
