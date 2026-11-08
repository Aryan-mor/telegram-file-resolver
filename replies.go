package fileresolver

import (
	"strings"
	"unicode/utf16"

	"github.com/gotd/td/tg"
)

// hasMedia reports a real attachment, not an empty media slot.
func hasMedia(m *tg.Message) bool {
	if m.Media == nil {
		return false
	}
	_, empty := m.Media.(*tg.MessageMediaEmpty)
	return !empty
}

// hasButtons reports any inline or reply keyboard.
func hasButtons(m *tg.Message) bool {
	switch mk := m.ReplyMarkup.(type) {
	case *tg.ReplyInlineMarkup:
		return len(mk.Rows) > 0
	case *tg.ReplyKeyboardMarkup:
		return len(mk.Rows) > 0
	}
	return false
}

// isTransient reports a bare wait indicator, such as "Please wait", that a bot edits or replaces
// with its real answer.
func isTransient(m *tg.Message, transient []string) bool {
	if hasMedia(m) || hasButtons(m) || len(transient) == 0 {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(m.Message))
	for _, t := range transient {
		if t != "" && strings.Contains(text, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// isBlocked reports a text-only message that contains a blocked word. Media with a caption is exempt.
func isBlocked(m *tg.Message, words []string) bool {
	if hasMedia(m) {
		return false
	}
	text := strings.ToLower(m.Message)
	for _, w := range words {
		if w != "" && strings.Contains(text, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// responseLinks collects Telegram links from the text, the formatted links inside it and the URL
// buttons of one message.
func responseLinks(m *tg.Message) []string {
	var out []string
	seen := map[string]bool{}
	add := func(raw string) { out = addLink(out, seen, raw) }
	for _, e := range m.Entities {
		switch ent := e.(type) {
		case *tg.MessageEntityTextURL:
			add(ent.URL)
		case *tg.MessageEntityURL:
			add(sliceUTF16(m.Message, ent.Offset, ent.Length))
		}
	}
	for _, l := range tmeLinks(m.Message) {
		add(l)
	}
	if mk, ok := m.ReplyMarkup.(*tg.ReplyInlineMarkup); ok {
		for _, row := range mk.Rows {
			for _, b := range row.Buttons {
				if btn, ok := b.Type.(*tg.InlineButtonTypeURL); ok {
					add(btn.URL)
				}
			}
		}
	}
	return out
}

// linksOf merges the links of several messages, keeping order and dropping repeats.
func linksOf(msgs []*tg.Message) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, l := range responseLinks(m) {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	return out
}

// sliceUTF16 cuts text by Telegram entity offsets, which count UTF-16 code units.
func sliceUTF16(text string, offset, length int) string {
	u := utf16.Encode([]rune(text))
	if offset < 0 || length < 0 || offset > len(u) {
		return ""
	}
	end := min(offset+length, len(u))
	return string(utf16.Decode(u[offset:end]))
}
