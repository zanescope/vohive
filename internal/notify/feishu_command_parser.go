package notify

import (
	"strings"
	"unicode/utf8"
)

func normalizeFeishuCommandText(text string, mentionKeys []string, requireMention bool) (string, bool) {
	text = strings.TrimSpace(text)
	mentionRemoved := false
	for {
		matched := false
		for _, rawKey := range mentionKeys {
			key := strings.TrimSpace(rawKey)
			if key == "" || !strings.HasPrefix(text, key) {
				continue
			}
			remaining := text[len(key):]
			if remaining != "" {
				first, _ := utf8.DecodeRuneInString(remaining)
				if !strings.ContainsRune(" \t\r\n", first) {
					continue
				}
			}
			text = strings.TrimSpace(remaining)
			mentionRemoved = true
			matched = true
			break
		}
		if !matched {
			break
		}
	}
	if requireMention && !mentionRemoved {
		return "", false
	}
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	return text, true
}
