package fileresolver

import (
	"slices"
	"testing"

	"github.com/gotd/td/tg"
)

func TestParseStartLink(t *testing.T) {
	good := map[string]string{
		"https://t.me/FileBot?start=abc_1":             "filebot|abc_1",
		"t.me/filebot/?start=x":                        "filebot|x",
		"http://www.telegram.me/filebot?foo=1&start=y": "filebot|y",
		"tg://resolve?domain=FileBot&start=z":          "filebot|z",
		"see https://t.me/filebot?start=k.":            "",
		"https://t.me/filebot?start=k.":                "filebot|k",
	}
	for in, want := range good {
		l, err := ParseStartLink(in)
		if want == "" {
			if err == nil {
				t.Errorf("%q should not parse", in)
			}
			continue
		}
		if err != nil || l.Bot+"|"+l.Argument != want {
			t.Errorf("%q = %+v, %v; want %s", in, l, err, want)
		}
	}
	for _, bad := range []string{"", "hello", "https://t.me/filebot", "https://t.me/filebot?start=", "https://t.me/abc?start=x", "https://t.me/a/b?start=x", "https://example.com/filebot?start=x"} {
		if _, err := ParseStartLink(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestJoinTarget(t *testing.T) {
	cases := []struct {
		in    string
		kind  joinKind
		value string
	}{
		{"https://t.me/+AbC123", joinInvite, "AbC123"},
		{"https://t.me/joinchat/AbC123", joinInvite, "AbC123"},
		{"https://t.me/somechannel", joinPublic, "somechannel"},
		{"https://t.me/somechannel?foo=1", joinPublic, "somechannel"},
		{"https://t.me/somebot?start=x", joinNone, ""},
		{"https://t.me/c/123/45", joinNone, ""},
		{"https://t.me/abc", joinNone, ""},
		{"https://example.com/somechannel", joinNone, ""},
	}
	for _, c := range cases {
		k, v := joinTarget(c.in)
		if k != c.kind || v != c.value {
			t.Errorf("%q = %v %q, want %v %q", c.in, k, v, c.kind, c.value)
		}
	}
}

func TestResponseLinksReadTextEntitiesAndButtons(t *testing.T) {
	m := &tg.Message{
		Message: "😀 open here and t.me/plainchan, thanks!",
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityTextURL{Offset: 3, Length: 4, URL: "https://t.me/+HIDDEN"},
		},
		ReplyMarkup: linkButton("https://t.me/btnchan"),
	}
	got := responseLinks(m)
	want := []string{"https://t.me/+HIDDEN", "https://t.me/plainchan", "https://t.me/btnchan"}
	if !slices.Equal(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

func TestSliceUTF16CountsCodeUnits(t *testing.T) {
	if got := sliceUTF16("😀 abc", 3, 3); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestTransientAndBlocked(t *testing.T) {
	spin := &tg.Message{Message: "  Please wait... "}
	if !isTransient(spin, []string{"please wait"}) {
		t.Fatal("spinner not detected")
	}
	if isTransient(&tg.Message{Message: "please wait", Media: &tg.MessageMediaDocument{}}, []string{"please wait"}) {
		t.Fatal("media is never a spinner")
	}
	if isTransient(&tg.Message{Message: "please wait", ReplyMarkup: linkButton("https://t.me/abcde")}, []string{"please wait"}) {
		t.Fatal("a message with buttons is never a spinner")
	}
	if !isBlocked(&tg.Message{Message: "Visit our SPONSOR"}, []string{"sponsor"}) {
		t.Fatal("blocked word missed")
	}
	if isBlocked(&tg.Message{Message: "sponsor", Media: &tg.MessageMediaDocument{}}, []string{"sponsor"}) {
		t.Fatal("captions of media are exempt")
	}
}
