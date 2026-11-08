# telegram-file-resolver

[![Go Reference](https://pkg.go.dev/badge/github.com/Aryan-mor/telegram-file-resolver.svg)](https://pkg.go.dev/github.com/Aryan-mor/telegram-file-resolver)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Give it a Telegram bot link such as `https://t.me/somebot?start=token`. It opens the bot, joins the
channels the bot says you must join, collects the files the bot finally sends, copies them
somewhere durable, and leaves those channels again later.

It is a Go library on [gotd](https://github.com/gotd/td) with two ways to run:

- **Embedded.** Your program already keeps a Telegram user session connected. You lend it to the
  library and call `Engine.Submit(ctx, link, opts)`. There is no second login, no second
  connection on the same auth key, and no message written into a chat to start work.
- **Standalone.** A small command logs in once (phone, code, and the two-step password if the
  account has one) and takes links from the command line or over HTTP.

It is a Go rewrite of the behaviour of [AmirHBuilds/fuckjoin](https://github.com/AmirHBuilds/fuckjoin)
(MIT), a Python/Telethon userbot that does the same job. No code is copied from it.

> It automates a **user account**, not a bot. Telegram limits how fast an account may join
> channels, and automating accounts can break Telegram's terms or get the account limited. Use it
> with an account you can afford to lose, only on content you may access, and at a gentle pace.

## Install

```
go get github.com/Aryan-mor/telegram-file-resolver
go install github.com/Aryan-mor/telegram-file-resolver/cmd/telegram-file-resolver@latest
```

Go 1.25 or newer. You need an `api_id` and `api_hash` from <https://my.telegram.org>.

## Use as a library

```go
// Provider lends a connected account. See "Embedding" below for a real one.
eng, err := fileresolver.New(provider, fileresolver.DefaultConfig())
if err != nil { return err }

go eng.Run(ctx) // leaves old channels in the background; cancel ctx to stop

res, err := eng.Submit(ctx, "https://t.me/somebot?start=token", fileresolver.Options{
    Progress: func(line string) { log.Println(line) },
    Sink:     fileresolver.ForwardSink{}, // copy into Saved Messages
})
var limited *fileresolver.RateLimitError
switch {
case errors.As(err, &limited):
    // Telegram refused joins for limited.Wait. Wait, then submit the same link again.
case err != nil:
    return err
}
for _, m := range res.Delivered.Messages { /* durable copies */ }
```

Set `cfg.StateDir` before `New` so joined channels survive a restart:

```go
cfg := fileresolver.DefaultConfig()
cfg.StateDir = "data/resolver"
```

### Results

| Field | Meaning |
|---|---|
| `Result.Messages` | What the last bot sent, after dropping protected, filtered and empty messages. They live in the bot chat and can disappear. |
| `Result.Delivered` | What the `Sink` passed on: `Count`, the new `Messages`, and the `Peer` they live in. **Keep these, not `Messages`.** Bots delete their messages after a few minutes, and `Cleanup` deletes the whole bot chat. |
| `Result.Joined`, `Handoffs`, `Failures` | Channels resolved, extra bots followed, and a note for each channel that could not be joined. |
| `Result.RateLimit` | The longest `FLOOD_WAIT` Telegram asked for while joining. |

### Errors

| Error | Meaning |
|---|---|
| `ErrNotStartLink` | The text is not a bot deep link with a `start` argument. |
| `*RateLimitError` | Joining was refused for `Wait` and nothing could be delivered. It is not the link's fault; retry later. |
| `ErrNoReply` | The bot sent nothing before `ResponseTimeout`. |
| `ErrNoContent` | The bot answered, but every message was protected, filtered or empty. |

### Embedding

Implement `Provider`. It lends an `API`, the subset of `*tg.Client` the library calls.

```go
provider := fileresolver.ProviderFunc(func(ctx context.Context, fn func(context.Context, fileresolver.API) error) error {
    return fn(ctx, tgClient) // a *tg.Client that is already connected
})
```

Two things matter when your program owns the connection:

- **Serialized calls.** If your program runs all Telegram calls through one loop, a whole flow in
  one `fn` would hold that loop for minutes. Hand out an `API` that makes one short call per RPC
  instead.
- **Several accounts.** Access hashes belong to one account. If your `API` has a `UserID() int64`
  method, the library stamps every joined chat with that id and only leaves chats of the account it
  is lent. Set `Config.Owner` when the `API` has no `UserID`.

### Sinks

`Sink` decides where the files go. `ForwardSink` forwards in chunks of 5 to a chat (Saved Messages
by default), waits out `FLOOD_WAIT`, and skips chunks the chat forbids copying. Write your own
`Sink` to upload, hash or hand the messages to another system; it returns a `Delivery` with the new
messages Telegram reported.

## Use the command

```
export API_ID=12345 API_HASH=abcdef...

telegram-file-resolver login
telegram-file-resolver submit "https://t.me/somebot?start=token"
telegram-file-resolver serve -addr 127.0.0.1:8787

curl -X POST localhost:8787/submit -d '{"link":"https://t.me/somebot?start=token"}'
```

Files are forwarded to Saved Messages of the logged-in account. The session and the list of joined
channels live in `-data` (default `./data`). `data/session.json` is a login for your account; keep
it private. `serve` has no authentication, so keep it on localhost.

## How it works

1. **Start.** Send `/start <argument>` to the bot and wait until its answer stops changing for
   `FollowupWindow`. Wait indicators such as "Please wait" are ignored, and a message the bot edits
   is read in its final form. A bot you blocked earlier is unblocked first.
2. **Join.** Collect every Telegram link from the text, the formatted links inside it and the URL
   buttons. Join public channels and private invites (`+hash`, `joinchat/hash`), including
   join-request links that the bot approves by itself.
3. **Repeat.** Send `/start` again after joining, up to `MaxStartAttempts` rounds, until the bot
   asks for nothing more.
4. **Hand off.** If the answer links to another bot, follow it, up to `MaxHandoffs`. The same bot
   and argument are never followed twice.
5. **Filter.** Drop protected messages, text-only messages that contain a blocked word, and empty
   ones. Media with a caption is never dropped for its caption.
6. **Deliver.** Pass the rest to the `Sink`.
7. **Leave.** After `LeaveAfter` (24 hours) the background sweep leaves each channel. If more than
   `MaxJoined` (100) are tracked, the older half goes. The list is kept in `StateDir`, so a restart
   still leaves them. `Engine.Cleanup` leaves everything now and also deletes and blocks the bots.

### Pace

Telegram limits joining, and a link can need several channels. The library spreads its waits:

- After each join it waits a random time between half and one and a half times the current pace
  (10 seconds is a good base; `DefaultConfig` uses 3).
- After a `FLOOD_WAIT` the pace doubles, or becomes a quarter of what Telegram asked for if that is
  larger, up to `PacingMax` (3 minutes). Every successful join eases it back toward the base.
- A `FLOOD_WAIT` up to 3 minutes is waited out with a little random extra. A longer one ends the
  link with `*RateLimitError`, so the host can decide when to try again.
- `contacts.resolveUsername` and `messages.getHistory` are retried on `FLOOD_WAIT`, and bot names
  are resolved only once.

### Configuration

Every field of `Config` has a default; start from `DefaultConfig()`.

| Field | Default | Meaning |
|---|---|---|
| `StateDir` | empty | Where joined channels and contacted bots are kept. Empty keeps them in memory. |
| `PacingDelay` | 3s | Base wait after a join. |
| `PacingMax` | 3m | Cap on the adaptive pace. |
| `RetryDelay` | 2s | Wait after joining before `/start` is sent again. |
| `LeaveAfter` | 24h | How long a joined channel stays. |
| `MaxJoined` | 100 | Tracked channels above which the older half is left. |
| `ResponseTimeout` | 30s | How long a bot may take to answer. |
| `FollowupWindow` | 2s | How long the answer must stay unchanged. |
| `MaxStartAttempts`, `MaxHandoffs` | 5, 5 | Bounds on one link. |
| `TransientTexts` | `...`, `please wait`, `processing...`, `⏳`, `⌛` | Wait indicators to ignore. |
| `BlockedWords` | none | Drop text-only messages containing one of these. |
| `Owner` | 0 | Account id when the `API` has no `UserID`. |
| `Log` | discard | A `*slog.Logger`. |

## Limits

- Inline **callback buttons** ("I joined ✅") are not clicked. Like the Python original, the library
  re-sends `/start` instead, so bots that only continue after a click are not supported yet.
- Links run **one at a time** per engine, because two flows on one account would mix up the replies.
- A join that needs a web verification step is reported as a failure.
- Whether a channel you already belong to is told apart from one you join now depends on the
  `Left` flag Telegram sends. It decides if a membership is tracked, so the sweep never leaves a
  channel you joined yourself. For public channels this is an assumption that has not been verified against real Telegram yet; private invites have been used with real bots.

## Development

```
go test -race ./...
```

The tests run the whole flow against a scripted fake of the Telegram API: join gates, join
requests, hand-offs, spinners that the bot edits, rate limits, the adaptive pace, the sweep and
cleanup. Nothing in them talks to Telegram.

## License

MIT. See [LICENSE](LICENSE). Inspired by [fuckjoin](https://github.com/AmirHBuilds/fuckjoin) by
AmirHBuilds (MIT).
