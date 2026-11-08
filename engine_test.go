package fileresolver

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// gatedBot answers /start with join requirements until both channels are joined, then with a video.
func gatedBot(bot int64, invite, public string) func(f *fake, b int64, text string) {
	return func(f *fake, b int64, text string) {
		if b != bot {
			return
		}
		if len(f.joined) < 2 {
			f.reply(bot, &tg.Message{
				Message:     "Join https://t.me/+" + invite + " first",
				ReplyMarkup: linkButton("https://t.me/" + public),
			})
			return
		}
		f.reply(bot, video("here is your file"))
	}
}

func TestSubmitJoinsChannelsAndReturnsVideo(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.invites["HASH1"] = &tg.Channel{ID: 500, AccessHash: 5, Left: true}
	f.channels["pubchan"] = &tg.Channel{ID: 600, AccessHash: 6, Left: true}
	f.script = gatedBot(100, "HASH1", "pubchan")

	eng, err := New(f.provider(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	res, err := eng.Submit(context.Background(), "https://t.me/FileBot?start=abc", Options{Progress: func(s string) { lines = append(lines, s) }})
	if err != nil {
		t.Fatalf("submit: %v (progress %v)", err, lines)
	}
	if res.Joined != 2 || len(res.Messages) != 1 || res.Bot != "filebot" {
		t.Fatalf("result = %+v", res)
	}
	if want := []string{"/start abc", "/start abc"}; !slices.Equal(f.sent, want) {
		t.Fatalf("sent = %v, want %v", f.sent, want)
	}
	if ch, bots := eng.Tracked(); ch != 2 || bots != 1 {
		t.Fatalf("tracked channels=%d bots=%d, want 2 and 1", ch, bots)
	}
}

func TestSubmitRejectsPlainText(t *testing.T) {
	eng, _ := New(newFake().provider(), DefaultConfig())
	if _, err := eng.Submit(context.Background(), "hello", Options{}); !errors.Is(err, ErrNotStartLink) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitWaitsOutSpinnerAndReadsTheEdit(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	var spinner *tg.Message
	polls := 0
	f.script = func(f *fake, b int64, text string) { spinner = f.reply(b, &tg.Message{Message: "Please wait..."}) }
	f.onHistory = func(f *fake, b int64) {
		polls++
		if polls == 3 && spinner != nil {
			spinner.Message, spinner.Media, spinner.EditDate = "your file", &tg.MessageMediaDocument{}, 1
		}
	}
	eng, _ := New(f.provider(), f.config())
	res, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{})
	if err != nil || len(res.Messages) != 1 || !hasMedia(res.Messages[0]) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSubmitFollowsHandoffAndNeverLoops(t *testing.T) {
	f := newFake()
	f.addBot("firstbot", 100)
	f.addBot("secondbot", 200)
	f.script = func(f *fake, b int64, text string) {
		switch b {
		case 100:
			f.reply(b, &tg.Message{Message: "Get it at https://t.me/secondbot?start=k1"})
		case 200:
			// Points back at the first bot: it must not be followed again.
			f.reply(b, &tg.Message{Message: "see https://t.me/firstbot?start=k0", Media: &tg.MessageMediaDocument{}})
		}
	}
	eng, _ := New(f.provider(), f.config())
	res, err := eng.Submit(context.Background(), "https://t.me/firstbot?start=k0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Bot != "secondbot" || res.Handoffs != 1 {
		t.Fatalf("bot=%q handoffs=%d", res.Bot, res.Handoffs)
	}
}

func TestSubmitTimesOutWhenBotIsSilent(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	eng, _ := New(f.provider(), f.config())
	if _, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{}); !errors.Is(err, ErrNoReply) {
		t.Fatalf("err = %v", err)
	}
}

func TestSubmitDropsProtectedAndFilteredMessages(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.script = func(f *fake, b int64, text string) {
		f.reply(b, &tg.Message{Message: "protected", Media: &tg.MessageMediaDocument{}, Noforwards: true})
		f.reply(b, &tg.Message{Message: "Sponsor: buy now"})
		f.reply(b, &tg.Message{Message: "   "})
	}
	cfg := f.config()
	cfg.BlockedWords = []string{"sponsor"}
	eng, _ := New(f.provider(), cfg)
	if _, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{}); !errors.Is(err, ErrNoContent) {
		t.Fatalf("err = %v", err)
	}
}

func TestJoinRequestLinkIsApprovedAndTracked(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.pending["REQ"] = &tg.Channel{ID: 700, AccessHash: 7, Left: true}
	f.script = func(f *fake, b int64, text string) {
		if len(f.joined) == 0 {
			f.reply(b, &tg.Message{Message: "https://t.me/+REQ"})
			return
		}
		f.reply(b, video("ok"))
	}
	eng, _ := New(f.provider(), f.config())
	res, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{})
	if err != nil || res.Joined != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if e, ok := eng.tr.entry("c:700"); !ok || e.AccessHash != 7 {
		t.Fatalf("join request was not tracked as a channel: %+v ok=%v", e, ok)
	}
}

func TestChannelAlreadyJoinedIsNotTracked(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.channels["mine1"] = &tg.Channel{ID: 800, AccessHash: 8, Left: false}
	f.script = func(f *fake, b int64, text string) {
		if len(f.hist[b]) == 0 {
			f.reply(b, &tg.Message{Message: "join @mine1 https://t.me/mine1"})
			return
		}
		f.reply(b, video("ok"))
	}
	eng, _ := New(f.provider(), f.config())
	if _, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{}); err != nil {
		t.Fatal(err)
	}
	if ch, _ := eng.Tracked(); ch != 0 {
		t.Fatalf("a channel the account was already in is tracked (%d)", ch)
	}
}

func TestBlockedBotIsUnblockedBeforeSending(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.blocked[100] = true
	f.script = func(f *fake, b int64, text string) { f.reply(b, video("ok")) }
	eng, _ := New(f.provider(), f.config())
	if _, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestSweepLeavesOldChannelsAndTheOlderHalfOnOverflow(t *testing.T) {
	f := newFake()
	cfg := f.config()
	cfg.StateDir = t.TempDir()
	cfg.MaxJoined = 4
	eng, _ := New(f.provider(), cfg)
	now := f.clock.Now()
	add := func(id int64, age time.Duration) {
		if err := eng.tr.recordJoin(joinedEntry{Kind: kindChannel, ID: id, AccessHash: id, At: now.Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	add(1, 30*time.Hour) // expired
	for i := range 6 {   // six fresh: over the limit of four, the older three go
		add(int64(10+i), time.Duration(6-i)*time.Hour)
	}
	if err := eng.tr.sweep(context.Background(), f, eng.cfg); err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 10, 11, 12}; !slices.Equal(f.left, want) {
		t.Fatalf("left = %v, want %v", f.left, want)
	}
	if ch, _ := eng.Tracked(); ch != 3 {
		t.Fatalf("tracked = %d, want 3", ch)
	}
	// A restart reads the same list from disk.
	again, err := New(f.provider(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ch, _ := again.Tracked(); ch != 3 {
		t.Fatalf("after restart tracked = %d, want 3", ch)
	}
}

func TestCleanupLeavesChannelsAndWipesBots(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.script = func(f *fake, b int64, text string) { f.reply(b, video("ok")) }
	eng, _ := New(f.provider(), f.config())
	if _, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{}); err != nil {
		t.Fatal(err)
	}
	_ = eng.tr.recordJoin(joinedEntry{Kind: kindChannel, ID: 9, AccessHash: 9, At: f.clock.Now()})
	res, err := eng.Cleanup(context.Background())
	if err != nil || res.ChannelsLeft != 1 || res.BotsCleaned != 1 || !f.blocked[100] {
		t.Fatalf("res=%+v err=%v blocked=%v", res, err, f.blocked)
	}
	if ch, bots := eng.Tracked(); ch != 0 || bots != 0 {
		t.Fatalf("still tracked: %d %d", ch, bots)
	}
}

func TestForwardSinkChunksAndRetriesAfterFlood(t *testing.T) {
	f := newFake()
	f.flood = 1
	var msgs []*tg.Message
	for i := 1; i <= 12; i++ {
		msgs = append(msgs, &tg.Message{ID: i})
	}
	sink := ForwardSink{Sleep: f.clock.Sleep}
	d, err := sink.Deliver(context.Background(), f, &tg.InputPeerUser{UserID: 1}, msgs)
	if err != nil || d.Count != 12 {
		t.Fatalf("count=%d err=%v", d.Count, err)
	}
	if len(f.forwards) != 3 || len(f.forwards[0]) != 5 || len(f.forwards[2]) != 2 {
		t.Fatalf("forwards = %v", f.forwards)
	}
}

func TestSubmitCancels(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	eng, _ := New(f.provider(), f.config())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.Submit(ctx, "https://t.me/filebot?start=x", Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestPollSurvivesFloodWaitAndBotIsResolvedOnce(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.script = func(f *fake, b int64, text string) { f.reply(b, video("ok")) }
	polls := 0
	f.onHistory = func(f *fake, b int64) { polls++ }
	eng, _ := New(&floodyProvider{inner: f.provider(), api: f, failHistory: 2}, f.config())
	for range 2 {
		if _, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if f.resolves != 1 {
		t.Fatalf("resolveUsername called %d times, want 1", f.resolves)
	}
}

// floodyProvider fails the first history reads with FLOOD_WAIT.
type floodyProvider struct {
	inner       Provider
	api         *fake
	failHistory int
}

func (p *floodyProvider) Run(ctx context.Context, fn func(context.Context, API) error) error {
	return fn(ctx, &floodyAPI{fake: p.api, p: p})
}

type floodyAPI struct {
	*fake
	p *floodyProvider
}

func (a *floodyAPI) MessagesGetHistory(ctx context.Context, req *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
	if a.p.failHistory > 0 {
		a.p.failHistory--
		return nil, tgerr.New(420, "FLOOD_WAIT_2")
	}
	return a.fake.MessagesGetHistory(ctx, req)
}

type ownedAPI struct {
	*fake
	id int64
}

func (o ownedAPI) UserID() int64 { return o.id }

func TestSweepOnlyLeavesChatsOfTheLentAccount(t *testing.T) {
	f := newFake()
	cur := int64(1)
	prov := ProviderFunc(func(ctx context.Context, fn func(context.Context, API) error) error {
		return fn(ctx, ownedAPI{fake: f, id: cur})
	})
	eng, _ := New(prov, f.config())
	old := f.clock.Now().Add(-48 * time.Hour)
	_ = eng.Provider().Run(context.Background(), func(ctx context.Context, api API) error {
		eng.tr.bind(api, 0)
		return eng.tr.recordJoin(joinedEntry{Kind: kindChannel, ID: 11, At: old})
	})
	cur = 2
	_ = eng.Provider().Run(context.Background(), func(ctx context.Context, api API) error {
		eng.tr.bind(api, 0)
		return eng.tr.recordJoin(joinedEntry{Kind: kindChannel, ID: 22, At: old})
	})
	if err := eng.Provider().Run(context.Background(), func(ctx context.Context, api API) error { eng.tr.bind(api, 0); return eng.tr.sweep(ctx, api, eng.cfg) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.left, []int64{22}) {
		t.Fatalf("left = %v, want only account 2's channel", f.left)
	}
	if _, ok := eng.tr.entry("c:11"); !ok {
		t.Fatal("the other account's channel must stay tracked for that account")
	}
}

func TestLongJoinFloodIsReportedAsRateLimitNotAsEmptyBot(t *testing.T) {
	f := newFake()
	f.addBot("filebot", 100)
	f.invites["HASH1"] = &tg.Channel{ID: 500, AccessHash: 5, Left: true}
	f.inviteFlood = 4 * time.Minute
	f.script = func(f *fake, b int64, text string) {
		f.reply(b, &tg.Message{Message: "join https://t.me/+HASH1"})
	}
	eng, _ := New(f.provider(), f.config())
	_, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Wait != 4*time.Minute {
		t.Fatalf("err = %v, want RateLimitError of 4m", err)
	}
	// After the wait the same link works.
	f.inviteFlood = 0
	f.script = gatedBot(100, "HASH1", "none")
	f.channels["none"] = &tg.Channel{ID: 600, AccessHash: 6, Left: true}
	if _, err := eng.Submit(context.Background(), "https://t.me/filebot?start=x", Options{}); err != nil {
		t.Fatalf("second try: %v", err)
	}
}

func TestPaceSpreadsSlowsAfterFloodAndEasesBack(t *testing.T) {
	f := newFake()
	cfg := f.config()
	cfg.PacingDelay = 10 * time.Second
	r := 0.0
	cfg.Rand = func() float64 { return r }
	eng, _ := New(f.provider(), cfg)

	if d := eng.nextPause(); d != 5*time.Second { // r=0 -> half the pace
		t.Fatalf("low end = %v", d)
	}
	r = 0.999
	if d := eng.nextPause(); d < 14*time.Second || d > 15*time.Second { // r~1 -> one and a half
		t.Fatalf("high end = %v", d)
	}

	eng.slowDown(4 * time.Minute) // doubles to 20s; a quarter of 4m is 60s, which is larger
	r = 0.5
	if d := eng.nextPause(); d != 60*time.Second {
		t.Fatalf("after flood = %v, want 60s", d)
	}
	for range 10 {
		eng.slowDown(10 * time.Minute)
	}
	if d := eng.nextPause(); d > 3*time.Minute {
		t.Fatalf("pause %v is above the 3 minute cap", d)
	}
	for range 40 {
		eng.speedUp()
	}
	if d := eng.nextPause(); d != 10*time.Second {
		t.Fatalf("after many joins = %v, want the base 10s", d)
	}
}
