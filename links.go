package fileresolver

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// StartLink is a bot deep link: https://t.me/<bot>?start=<argument>.
type StartLink struct {
	Bot      string
	Argument string
	URL      string
}

// Key identifies the bot and argument, so a hand-off never loops back to a link already followed.
func (s StartLink) Key() string { return s.Bot + "\x00" + s.Argument }

// ErrNotStartLink is returned for text that is not a bot deep link with a start argument.
var ErrNotStartLink = errors.New("use a link like https://t.me/bot_name?start=argument")

var (
	reHost   = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?(?:t\.me|telegram\.me|telegram\.dog)/`)
	reName   = regexp.MustCompile(`^[A-Za-z0-9_]{5,}$`)
	reTMe    = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?(?:t\.me|telegram\.me|telegram\.dog)/[^\s<>()\[\]"']+`)
	reHandle = regexp.MustCompile(`^[A-Za-z0-9_]{5,}$`)
)

// ParseStartLink accepts t.me, telegram.me and telegram.dog links, with or without a scheme, and
// the tg://resolve?domain=<bot>&start=<argument> form Telegram itself writes.
func ParseStartLink(raw string) (StartLink, error) {
	raw = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(raw), ".,;:!?"))
	if strings.HasPrefix(strings.ToLower(raw), "tg://") {
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Host, "resolve") {
			return StartLink{}, ErrNotStartLink
		}
		q := u.Query()
		return buildStart(q.Get("domain"), q.Get("start"))
	}
	loc := reHost.FindStringIndex(raw)
	if loc == nil {
		return StartLink{}, ErrNotStartLink
	}
	rest := raw[loc[1]:]
	path, query, found := strings.Cut(rest, "?")
	path = strings.Trim(path, "/")
	if !found || strings.Contains(path, "/") {
		return StartLink{}, ErrNotStartLink
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return StartLink{}, ErrNotStartLink
	}
	return buildStart(path, q.Get("start"))
}

func buildStart(bot, arg string) (StartLink, error) {
	if !reName.MatchString(bot) || arg == "" {
		return StartLink{}, ErrNotStartLink
	}
	bot = strings.ToLower(bot)
	return StartLink{Bot: bot, Argument: arg, URL: "https://t.me/" + bot + "?start=" + url.QueryEscape(arg)}, nil
}

// tmeLinks returns the Telegram links in text, in order, without repeats.
func tmeLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reTMe.FindAllString(text, -1) {
		out = addLink(out, seen, m)
	}
	return out
}

func addLink(out []string, seen map[string]bool, raw string) []string {
	link := normalizeLink(raw)
	if link == "" || seen[link] {
		return out
	}
	seen[link] = true
	return append(out, link)
}

func normalizeLink(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), ".,;:!?")
	if un, err := url.PathUnescape(raw); err == nil {
		raw = un
	}
	if strings.HasPrefix(strings.ToLower(raw), "tg://") {
		return raw
	}
	loc := reHost.FindStringIndex(raw)
	if loc == nil {
		return ""
	}
	return "https://t.me/" + raw[loc[1]:]
}

// joinKind says how a link is joined.
type joinKind int

const (
	joinNone joinKind = iota
	joinInvite
	joinPublic
)

// joinTarget classifies a link as a private invite (t.me/+hash, t.me/joinchat/hash) or a public
// channel name. A bot deep link is not a join target.
func joinTarget(link string) (joinKind, string) {
	if _, err := ParseStartLink(link); err == nil {
		return joinNone, ""
	}
	loc := reHost.FindStringIndex(link)
	if loc == nil {
		return joinNone, ""
	}
	path, _, _ := strings.Cut(link[loc[1]:], "?")
	path, _, _ = strings.Cut(path, "#")
	path = strings.Trim(path, "/")
	switch {
	case strings.HasPrefix(path, "+") && len(path) > 1 && !strings.Contains(path, "/"):
		return joinInvite, path[1:]
	case strings.HasPrefix(strings.ToLower(path), "joinchat/"):
		hash := path[len("joinchat/"):]
		if hash == "" || strings.Contains(hash, "/") {
			return joinNone, ""
		}
		return joinInvite, hash
	case reHandle.MatchString(path):
		return joinPublic, path
	}
	return joinNone, ""
}
