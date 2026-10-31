package main

import (
	"strings"
	"testing"
)

const normSkillA = `  <skill>
    <name>demo-skill</name>
    <description>Does demo things on Linux hosts.</description>
    <location>/home/user/.agents/skills/demo-skill/SKILL.md</location>
  </skill>`

const normSkillB = `  <skill>
    <name>demo-skill</name>
    <description>Does demo things on Kubernetes hosts with extra steps.</description>
    <location>/home/user/.claude/skills/demo-skill-variant/SKILL.md</location>
  </skill>`

const normSkillUUID = `  <skill>
    <name>trash-skill</name>
    <description>Synced copy.</description>
    <location>/home/user/.config/skills/synced/0779e573-ed94-4477-8300-c6269e538d88_SKILL.md</location>
  </skill>`

func normSystem(skills string, reminder bool) message {
	sys := "You are a coding agent.\n<available_skills>\n" + skills + "\n</available_skills>"
	if reminder {
		sys += "\nYou operate in a context-constrained environment.\nUse compress wisely.\n<dcp-system-reminder>\nDo not include task content.\n</dcp-system-reminder>\nTail line."
	} else {
		sys += "\nTail line."
	}
	return message{Role: "system", Content: sys}
}

func TestNormalizeVolatileSkillMetadata(t *testing.T) {
	user := message{Role: "user", Content: "hello"}
	saved := []message{normSystem(normSkillA+"\n"+normSkillUUID, false), user}
	incoming := []message{normSystem(normSkillB+"\n"+normSkillUUID, true), user}
	// The UUID location also flips spelling to prove shape-independence.
	incoming[0].Content = strings.ReplaceAll(incoming[0].Content.(string),
		"/home/user/.config/skills/synced/0779e573-ed94-4477-8300-c6269e538d88_SKILL.md",
		"/home/user/.agents/skills/.trash/1790677999082-65550-abc/SKILL.md")
	if got := firstMismatch(incoming, saved); got != nil {
		t.Fatalf("volatile skill metadata must match: %+v", got)
	}
}

func TestNormalizeStillDetectsGenuineChanges(t *testing.T) {
	user := message{Role: "user", Content: "hello"}
	base := []message{normSystem(normSkillA, false), user}
	tests := []struct {
		name   string
		change func(*message)
	}{
		{"skill name", func(m *message) {
			m.Content = strings.Replace(m.Content.(string), "<name>demo-skill</name>", "<name>other-skill</name>", 1)
		}},
		{"agent prompt", func(m *message) {
			m.Content = strings.Replace(m.Content.(string), "You are a coding agent.", "You are a writing agent.", 1)
		}},
		{"tail text", func(m *message) {
			m.Content = strings.Replace(m.Content.(string), "Tail line.", "Tail line edited.", 1)
		}},
		{"location outside skill block", func(m *message) {
			m.Content = m.Content.(string) + "\n<location>/elsewhere</location>"
		}},
		{"description outside skill block", func(m *message) {
			m.Content = m.Content.(string) + "\n<description>free text</description>"
		}},
		{"unclosed reminder left exact", func(m *message) {
			m.Content = m.Content.(string) + "\nYou operate in a context-constrained environment.\n(no closer, must stay exact)"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := append([]message{}, base...)
			sys := changed[0]
			tt.change(&sys)
			changed[0] = sys
			if got := firstMismatch(changed, base); got == nil {
				t.Fatal("genuine change must still mismatch")
			}
		})
	}
}

func TestNormalizeDCPMessageID(t *testing.T) {
	mkUser := func(id string) message {
		return message{Role: "user", Content: "Do the thing.\n\n<dcp-message-id>" + id + "</dcp-message-id>"}
	}
	mkTool := func(id string) message {
		return message{Role: "tool", Content: "result body\n<dcp-message-id>" + id + "</dcp-message-id>", ToolCallID: "call_1"}
	}
	sys := normSystem(normSkillA, false)
	asst := message{Role: "assistant", Content: "done"}
	saved := []message{sys, mkUser("m0022"), asst, mkTool("m0023")}
	incoming := []message{sys, mkUser("m0043"), asst, mkTool("m0044")}
	if got := firstMismatch(incoming, saved); got != nil {
		t.Fatalf("fresh platform message IDs must match: %+v", got)
	}
	edited := []message{sys, mkUser("m0043"), asst,
		{Role: "tool", Content: "CHANGED body\n<dcp-message-id>m0044</dcp-message-id>", ToolCallID: "call_1"}}
	if got := firstMismatch(edited, saved); got == nil {
		t.Fatal("genuine tool-result edit must still mismatch")
	}
	editedUser := []message{sys,
		{Role: "user", Content: "Do the other thing.\n\n<dcp-message-id>m0043</dcp-message-id>"}, asst, mkTool("m0044")}
	if got := firstMismatch(editedUser, saved); got == nil {
		t.Fatal("genuine user edit must still mismatch")
	}
}

func TestNormalizeAssistantEnvelope(t *testing.T) {
	envelope := func(answer, id string) string {
		return answer + "\n\n<dcp-system-reminder>\nPlatform boilerplate.\n</dcp-system-reminder>\n<dcp-message-id>" + id + "</dcp-message-id>"
	}
	sys := normSystem(normSkillA, false)
	user := message{Role: "user", Content: "hi"}
	// Bridge stores the bare answer; OpenCode re-emits it enveloped.
	saved := []message{sys, user, {Role: "assistant", Content: "GOT IT"}}
	incoming := []message{sys, user, {Role: "assistant", Content: envelope("GOT IT", "m0109")}}
	if got := firstMismatch(incoming, saved); got != nil {
		t.Fatalf("assistant envelope must match bare answer: %+v", got)
	}
	changed := []message{sys, user, {Role: "assistant", Content: envelope("GOT OTHER", "m0109")}}
	if got := firstMismatch(changed, saved); got == nil {
		t.Fatal("changed answer inside envelope must still mismatch")
	}
	// The envelope strip is assistant-gated: user text stays exact.
	userEnv := []message{sys, {Role: "user", Content: envelope("hi", "m1")}}
	if got := firstMismatch(userEnv, []message{sys, user}); got == nil {
		t.Fatal("envelope in user role must still mismatch")
	}
}

func TestNormalizeHistoryStillGuards(t *testing.T) {
	sys := normSystem(normSkillA, false)
	user := message{Role: "user", Content: "hello"}
	edited := message{Role: "user", Content: "hello edited"}
	if got := firstMismatch([]message{sys, edited}, []message{sys, user}); got == nil {
		t.Fatal("user edit must still mismatch")
	}
	if got := firstMismatch([]message{sys}, []message{sys, user}); got == nil {
		t.Fatal("truncation must still mismatch")
	}
}
