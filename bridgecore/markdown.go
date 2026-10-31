package bridgecore

import (
	"html"
	"regexp"
	"strings"
)

// AnswerToMarkdown converts one finished answer to Markdown for delivery.
// Wrapper-bearing answers bypass conversion untouched.
func AnswerToMarkdown(htmlFrag, plain string) string {
	if strings.Contains(plain, "<opencode_bridge_call") {
		return plain
	}
	md := HTMLToMarkdownFrag(htmlFrag)
	if strings.TrimSpace(md) == "" {
		return plain
	}
	return strings.TrimSpace(md)
}

func stripFragTags(s string) string {
	return regexp.MustCompile(`(?s)<[^>]*>`).ReplaceAllString(s, "")
}

// HTMLToMarkdownFrag converts an HTML fragment to GitHub-flavored Markdown.
func HTMLToMarkdownFrag(s string) string {
	s = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<button[^>]*>.*?</button>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<[^>]*role="toolbar"[^>]*>.*?</[^>]+>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<[^>]*(aria-hidden="true"|data-thought|data-thinking)[^>]*>.*?</[^>]+>`).ReplaceAllString(s, "")

	s = regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`).ReplaceAllStringFunc(s, func(m string) string {
		code := regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`).FindStringSubmatch(m)[1]
		code = html.UnescapeString(stripFragTags(code))
		fence := "```"
		if strings.Contains(code, "```") {
			fence = "````"
		}
		return "\n" + fence + "\n" + strings.Trim(code, "\n") + "\n" + fence + "\n"
	})

	for i := 6; i >= 1; i-- {
		tag := string(rune('0' + i))
		re := regexp.MustCompile(`(?s)<h` + tag + `[^>]*>(.*?)</h` + tag + `>`)
		prefix := strings.Repeat("#", i) + " "
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			inner := strings.TrimSpace(stripFragTags(re.FindStringSubmatch(m)[1]))
			return "\n" + prefix + inner + "\n"
		})
	}

	s = regexp.MustCompile(`(?s)<(strong|b)[^>]*>(.*?)</(strong|b)>`).ReplaceAllString(s, "**$2**")
	s = regexp.MustCompile(`(?s)<(em|i)[^>]*>(.*?)</(em|i)>`).ReplaceAllString(s, "*$2*")
	s = regexp.MustCompile(`(?s)<(del|s|strike)[^>]*>(.*?)</(del|s|strike)>`).ReplaceAllString(s, "~~$2~~")
	s = regexp.MustCompile(`(?s)<code[^>]*>(.*?)</code>`).ReplaceAllStringFunc(s, func(m string) string {
		inner := html.EscapeString(strings.TrimSpace(stripFragTags(regexp.MustCompile(`(?s)<code[^>]*>(.*?)</code>`).FindStringSubmatch(m)[1])))
		if strings.Contains(inner, "`") {
			return "`` " + inner + " ``"
		}
		return "`" + inner + "`"
	})

	s = regexp.MustCompile(`<a[^>]*href="([^"]*)"[^>]*>(.*?)</a>`).ReplaceAllStringFunc(s, func(m string) string {
		parts := regexp.MustCompile(`<a[^>]*href="([^"]*)"[^>]*>(.*?)</a>`).FindStringSubmatch(m)
		text := strings.TrimSpace(stripFragTags(parts[2]))
		url := strings.TrimSpace(parts[1])
		if text == "" {
			return url
		}
		return "[" + text + "](" + url + ")"
	})

	s = regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`).ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(stripFragTags(regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`).FindStringSubmatch(m)[1]))
		return "\n- " + inner
	})
	s = regexp.MustCompile(`(?s)<(ul|ol)[^>]*>(.*?)</(ul|ol)>`).ReplaceAllString(s, "\n$2\n")
	s = regexp.MustCompile(`(?s)<blockquote[^>]*>(.*?)</blockquote>`).ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(stripFragTags(regexp.MustCompile(`(?s)<blockquote[^>]*>(.*?)</blockquote>`).FindStringSubmatch(m)[1]))
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			lines[i] = "> " + strings.TrimSpace(l)
		}
		return "\n" + strings.Join(lines, "\n") + "\n"
	})

	s = regexp.MustCompile(`(?s)<(p|div|section|article)[^>]*>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?s)</(p|div|section|article)[^>]*>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?s)<br[^>]*>`).ReplaceAllString(s, "\n")
	s = stripFragTags(s)
	s = html.UnescapeString(s)
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
