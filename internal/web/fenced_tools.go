package web

import (
	"encoding/json"
	"regexp"
	"strings"
)

var fencedToolCall = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)\\s*\\n(.*?)\\n```")

// illustrativeProseLimit is the amount of natural-language text (outside any
// fenced block) beyond which a bare ```bash/sh/...``` block is treated as an
// illustrative example rather than an execution request. A genuine "run this"
// answer from the upstream model carries little surrounding prose; a normal
// chat answer that merely quotes a shell command carries a lot. Without this
// guard an explanatory answer containing a code sample was converted into a
// tool call and its entire prose body was discarded (content:nil), so the
// client saw a truncated / empty reply while the upstream conversation held
// the full text. The limit is measured in runes so CJK answers (which are
// byte-dense but rune-compact) are judged fairly.
const illustrativeProseLimit = 120

// declaredShell returns the shell-ish tool name the client actually
// declared (bash/sh/shell/powershell/cmd), or "" if none. Forcing an
// undeclared bash call on clients that don't support it (issue #12) makes
// them error out and loop, so conversion only happens for declared tools.
func declaredShell(allowed map[string]bool) string {
	for _, n := range []string{"bash", "sh", "shell", "powershell", "cmd"} {
		if allowed[n] {
			return n
		}
	}
	return ""
}

// proseOutsideFences returns the length in runes of the text left after all
// fenced code blocks are removed. It measures how much explanatory prose
// surrounds the code, which distinguishes an example from an execution intent.
func proseOutsideFences(text string) int {
	stripped := fencedToolCall.ReplaceAllString(text, "")
	return len([]rune(strings.TrimSpace(stripped)))
}

func fencedToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	allowed := allowedToolNames(tools)
	shell := declaredShell(allowed)
	// A shell code block wrapped in a long prose answer is an illustration, not
	// a command to run. Explicit ```toolname\n{json}``` blocks (where the fence
	// language is the declared tool name) stay unambiguous and are unaffected.
	illustrative := proseOutsideFences(text) > illustrativeProseLimit
	var out []detectedToolCall
	for _, m := range fencedToolCall.FindAllStringSubmatch(text, -1) {
		name := m[1]
		args := strings.TrimSpace(m[2])
		var v any
		_ = json.Unmarshal([]byte(args), &v)
		// Auto-convert bash/shell code blocks to tool calls, but only when
		// the client declared the tool.
		if name == "bash" || name == "sh" || name == "shell" || name == "powershell" || name == "cmd" {
			// A shell block embedded in a long explanatory answer is a quoted
			// example, not a command to execute. Skip conversion so the prose
			// answer is delivered intact instead of being replaced by an
			// (unwanted) tool call with content:nil.
			if illustrative {
				continue
			}
			converted := name
			if !allowed[name] {
				if shell == "" {
					continue
				}
				converted = shell
			}
			if m, ok := v.(map[string]any); ok {
				if cmd, hasCmd := m["command"]; hasCmd && cmd != "" {
					cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": m["timeout"], "workdir": m["workdir"]})
					out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
					continue
				}
			}
			if v == nil {
				cmdBytes, _ := json.Marshal(map[string]any{"command": args})
				out = append(out, detectedToolCall{ID: callID(converted, string(cmdBytes), len(out)), Type: "function", Name: converted, Arguments: cmdBytes})
				continue
			}
			continue
		}
		if !allowed[name] || !toolChoiceAllows(choice, name) {
			continue
		}
		if v == nil {
			continue
		}
		b, _ := json.Marshal(v)
		out = append(out, detectedToolCall{ID: callID(name, string(b), len(out)), Type: toolType(name, tools), Name: name, Arguments: b})
	}
	// Also check for plain JSON objects with a "command" field (not in fenced blocks)
	if len(out) == 0 && shell != "" && !illustrative {
		for i := 0; i < len(text); i++ {
			if text[i] != '{' {
				continue
			}
			end := strings.Index(text[i:], "\n")
			if end < 0 {
				end = len(text) - i
			}
			line := text[i : i+end]
			braceEnd := strings.LastIndex(line, "}")
			if braceEnd < 0 {
				continue
			}
			if !strings.Contains(line[:braceEnd+1], `"command"`) {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(line[:braceEnd+1]), &obj) != nil {
				continue
			}
			if cmd, hasCmd := obj["command"]; hasCmd && cmd != "" {
				cmdBytes, _ := json.Marshal(map[string]any{"command": cmd, "timeout": obj["timeout"], "workdir": obj["workdir"]})
				out = append(out, detectedToolCall{ID: callID(shell, string(cmdBytes), len(out)), Type: "function", Name: shell, Arguments: cmdBytes})
				break
			}
		}
	}
	return out
}
