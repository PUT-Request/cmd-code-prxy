package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

// fakePNG builds a data URL of roughly the requested base64 length. The bytes
// do not matter; what is asserted is whether it is treated as an image or text.
func fakePNG(bytes int) string {
	return "data:image/png;base64," + strings.Repeat("A", bytes)
}

func partOfType(msgs []api.CCMessage, typ string) []api.CCContentPart {
	var out []api.CCContentPart
	for _, m := range msgs {
		for _, p := range m.Content {
			if p.Type == typ {
				out = append(out, p)
			}
		}
	}
	return out
}

func roles(msgs []api.CCMessage) string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return strings.Join(out, ",")
}

// A data URL in a user message must become a real CC image part with a mimeType,
// not text.
func TestConvertMessagesUserImageBecomesImagePart(t *testing.T) {
	ccMsgs := ConvertMessages([]api.OpenAIMessage{{
		Role: "user",
		Content: []any{
			map[string]any{"type": "text", "text": "what is this"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": fakePNG(1024)}},
		},
	}})

	images := partOfType(ccMsgs, "image")
	if len(images) != 1 {
		t.Fatalf("expected 1 image part, got %d (parts: %+v)", len(images), ccMsgs[0].Content)
	}
	if images[0].Image == nil || !strings.HasPrefix(*images[0].Image, "data:image/png;base64,") {
		t.Errorf("image payload must be the data URL verbatim, got %+v", images[0].Image)
	}
	if images[0].MimeType == nil || *images[0].MimeType != "image/png" {
		t.Errorf("mimeType should come from the data URL, got %+v", images[0].MimeType)
	}
}

// The core production bug: images in tool output must never be tokenized as
// text. A single 2.76MB screenshot as text is ~1.92M tokens and blows the 1M
// context window.
func TestConvertMessagesToolImageHoistedOutOfToolResult(t *testing.T) {
	ccMsgs := ConvertMessages([]api.OpenAIMessage{
		{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID: "call_1", Type: "function",
			Function: api.FunctionCall{Name: "screenshot", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: []any{
			map[string]any{"type": "input_image", "image_url": fakePNG(300 * 1024)},
		}},
	})

	// tool message must exist but carry no base64
	var toolMsg *api.CCMessage
	for i := range ccMsgs {
		if ccMsgs[i].Role == "tool" {
			toolMsg = &ccMsgs[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("tool result message must still be sent, roles=%s", roles(ccMsgs))
	}
	if toolMsg != nil {
		for _, p := range toolMsg.Content {
			if p.Output != nil && strings.Contains(p.Output.Value, "data:image") {
				t.Fatalf("base64 leaked into tool-result text: %q", p.Output.Value)
			}
		}
	}

	images := partOfType(ccMsgs, "image")
	if len(images) != 1 {
		t.Fatalf("expected 1 hoisted image, got %d", len(images))
	}

	// Image must live on a user message immediately after the tool message.
	toolIdx, imgIdx := -1, -1
	for i, m := range ccMsgs {
		if m.Role == "tool" {
			toolIdx = i
		}
		for _, p := range m.Content {
			if p.Type == "image" {
				imgIdx = i
			}
		}
	}
	if imgIdx != toolIdx+1 {
		t.Errorf("image message must directly follow the tool result (tool=%d img=%d)", toolIdx, imgIdx)
	}
	if ccMsgs[imgIdx].Role != "user" {
		t.Errorf("hoisted image must be on a user message, got role %q", ccMsgs[imgIdx].Role)
	}
}

// A large data URL embedded in tool output *text* must be extracted too.
func TestConvertMessagesInlineDataURLInToolText(t *testing.T) {
	url := fakePNG(300 * 1024)
	ccMsgs := ConvertMessages([]api.OpenAIMessage{
		{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID: "call_1", Type: "function",
			Function: api.FunctionCall{Name: "screenshot", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: "size 2.76MB\n" + url},
	})

	var toolVal string
	for _, m := range ccMsgs {
		if m.Role == "tool" {
			for _, p := range m.Content {
				if p.Output != nil {
					toolVal = p.Output.Value
				}
			}
		}
	}
	if strings.Contains(toolVal, "data:image") {
		t.Fatalf("inline base64 must be stripped from text: %q", toolVal)
	}
	if !strings.Contains(toolVal, "[image]") {
		t.Errorf("placeholder should remain so the model knows an image was there, got %q", toolVal)
	}
	if len(partOfType(ccMsgs, "image")) != 1 {
		t.Errorf("inline image should be hoisted as a real image part")
	}
}

// Regression: plain text tool output must not gain extra messages.
func TestConvertMessagesPlainTextToolOutputUnchanged(t *testing.T) {
	text := "Chunk ID: 1\nProcess exited with code 0\nOutput:\nhello"
	ccMsgs := ConvertMessages([]api.OpenAIMessage{
		{Role: "user", Content: "run it"},
		{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID: "call_1", Type: "function",
			Function: api.FunctionCall{Name: "bash", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: text},
	})

	if got := roles(ccMsgs); got != "user,assistant,tool" {
		t.Errorf("no extra messages should be inserted, got roles %s", got)
	}
	for _, m := range ccMsgs {
		if m.Role != "tool" {
			continue
		}
		if len(m.Content) != 1 || m.Content[0].Output == nil {
			t.Fatalf("expected a single tool-result part, got %+v", m.Content)
		}
		if m.Content[0].Output.Value != text {
			t.Errorf("text must pass through unchanged:\n got %q\nwant %q", m.Content[0].Output.Value, text)
		}
	}
}

// The budget keeps the newest images and leaves a placeholder for dropped ones,
// so the model does not think the screenshot never existed.
func TestTrimToolImagesBudgetKeepsNewest(t *testing.T) {
	orig := maxToolImageBytes
	maxToolImageBytes = 1024 * 1024 // 1MB budget
	defer func() { maxToolImageBytes = orig }()

	img := fakePNG(400 * 1024) // ~0.4MB each; 3 of them exceed a 1MB budget

	var msgs []api.CCMessage
	for i := 0; i < 3; i++ {
		msgs = append(msgs, api.CCMessage{Role: "tool", Content: []api.CCContentPart{{
			Type:       "tool-result",
			ToolCallID: strPtr("call_" + string(rune('a'+i))),
			Output:     &api.CCToolOutput{Type: "text", Value: "ok"},
		}}})
		msgs = append(msgs, imageHoistMessage([]string{img}))
	}

	TrimToolImages(msgs)

	if got := len(partOfType(msgs, "image")); got != 2 {
		t.Errorf("1MB budget should keep the newest 2 images, kept %d", got)
	}
	placeholders := 0
	for _, m := range msgs {
		for _, p := range m.Content {
			if p.Type == "text" && p.Text != nil && strings.Contains(*p.Text, "image budget exceeded") {
				placeholders++
			}
		}
	}
	if placeholders != 1 {
		t.Errorf("dropped image needs a placeholder, found %d", placeholders)
	}
}

func TestTrimToolImagesDisabledByEnv(t *testing.T) {
	orig := maxToolImageBytes
	maxToolImageBytes = 0
	defer func() { maxToolImageBytes = orig }()

	img := fakePNG(400 * 1024)
	var msgs []api.CCMessage
	for i := 0; i < 3; i++ {
		msgs = append(msgs, imageHoistMessage([]string{img}))
	}
	TrimToolImages(msgs)
	if got := len(partOfType(msgs, "image")); got != 3 {
		t.Errorf("budget disabled must keep all images, kept %d", got)
	}
}

// Reasoning must lead the assistant content as a real reasoning block: CC
// rejects requests whose thinking is missing from the history.
func TestConvertMessagesReasoningLeadsAssistantParts(t *testing.T) {
	ccMsgs := ConvertMessages([]api.OpenAIMessage{{
		Role:             "assistant",
		ReasoningContent: strPtr("let me think"),
		Content:          "here is the answer",
		ToolCalls: []api.ToolCall{{
			ID: "call_1", Type: "function",
			Function: api.FunctionCall{Name: "bash", Arguments: "{}"},
		}},
	}})

	parts := ccMsgs[0].Content
	if len(parts) < 3 {
		t.Fatalf("expected reasoning + text + tool-call, got %+v", parts)
	}
	if parts[0].Type != "reasoning" {
		t.Errorf("reasoning must come first, got %q", parts[0].Type)
	}
	if parts[0].Text == nil || *parts[0].Text != "let me think" {
		t.Errorf("reasoning text lost: %+v", parts[0])
	}
	var sawText, sawTool bool
	for _, p := range parts[1:] {
		if p.Type == "text" {
			sawText = true
		}
		if p.Type == "tool-call" {
			sawTool = true
		}
	}
	if !sawText || !sawTool {
		t.Errorf("text and tool-call must follow reasoning, got %+v", parts)
	}
}

// An image-only tool result must still emit a tool-result block: dropping it
// breaks the tool_call_id linkage the upstream validates.
func TestConvertMessagesImageOnlyToolKeepsToolResult(t *testing.T) {
	ccMsgs := ConvertMessages([]api.OpenAIMessage{
		{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID: "call_1", Type: "function",
			Function: api.FunctionCall{Name: "screenshot", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: []any{
			map[string]any{"type": "input_image", "image_url": fakePNG(300 * 1024)},
		}},
	})

	var tool *api.CCMessage
	for i := range ccMsgs {
		if ccMsgs[i].Role == "tool" {
			tool = &ccMsgs[i]
		}
	}
	if tool == nil {
		t.Fatalf("tool message missing, roles=%s", roles(ccMsgs))
	}
	if len(tool.Content) != 1 || tool.Content[0].Type != "tool-result" {
		t.Fatalf("tool message must carry exactly one tool-result part, got %+v", tool.Content)
	}
	if tool.Content[0].ToolCallID == nil || *tool.Content[0].ToolCallID != "call_1" {
		t.Errorf("tool-result must keep its toolCallId, got %+v", tool.Content[0].ToolCallID)
	}
	if tool.Content[0].Output == nil {
		t.Fatal("tool-result must carry an output block")
	}
}

// A reasoning block in the content array must not be duplicated when
// reasoning_content is also present.
func TestConvertMessagesNoDuplicateReasoning(t *testing.T) {
	ccMsgs := ConvertMessages([]api.OpenAIMessage{{
		Role:             "assistant",
		ReasoningContent: strPtr("thinking hard"),
		Content: []any{
			map[string]any{"type": "reasoning", "text": "thinking hard"},
			map[string]any{"type": "text", "text": "done"},
		},
	}})

	n := 0
	for _, p := range ccMsgs[0].Content {
		if p.Type == "reasoning" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly 1 reasoning block, got %d (%+v)", n, ccMsgs[0].Content)
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	for _, in := range []string{"low", "LOW", " medium ", "high", "max", "minimal"} {
		if got := NormalizeReasoningEffort(in); got == "" {
			t.Errorf("expected %q to be accepted", in)
		}
	}
	for _, in := range []string{"", "  ", "extreme", "thinking"} {
		if got := NormalizeReasoningEffort(in); got != "" {
			t.Errorf("expected %q to be dropped, got %q", in, got)
		}
	}
}

func TestBuildRequestSetsReasoningEffort(t *testing.T) {
	p := NewProxy(nil)

	effort := "high"
	body, err := p.BuildRequest(api.OpenAIChatRequest{
		Model:           "deepseek/deepseek-v4-flash",
		Messages:        []api.OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: &effort,
	})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if body.Params.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort should be forwarded, got %q", body.Params.ReasoningEffort)
	}

	bad := "extreme"
	body, _ = p.BuildRequest(api.OpenAIChatRequest{
		Model:           "deepseek/deepseek-v4-flash",
		Messages:        []api.OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: &bad,
	})
	if body.Params.ReasoningEffort != "" {
		t.Errorf("invalid effort should be omitted, got %q", body.Params.ReasoningEffort)
	}
}

// reasoning_effort must be omitted from the wire when unset.
func TestBuildRequestOmitsEmptyReasoningEffort(t *testing.T) {
	p := NewProxy(nil)
	body, _ := p.BuildRequest(api.OpenAIChatRequest{
		Model:    "deepseek/deepseek-v4-flash",
		Messages: []api.OpenAIMessage{{Role: "user", Content: "hi"}},
	})
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "reasoning_effort") {
		t.Errorf("reasoning_effort should be absent from the wire when unset: %s", data)
	}
}

func TestResponsesReasoningEffortMapped(t *testing.T) {
	req := api.OpenAIResponsesRequest{
		Model:     "deepseek/deepseek-v4-flash",
		Input:     "hi",
		Reasoning: &api.ResponsesAPIReasoning{Effort: "max"},
	}
	chat := responsesToChatRequest(req)
	if chat.ReasoningEffort == nil || *chat.ReasoningEffort != "max" {
		t.Errorf("responses reasoning.effort should map to reasoning_effort, got %+v", chat.ReasoningEffort)
	}

	req.Reasoning = nil
	if chat := responsesToChatRequest(req); chat.ReasoningEffort != nil {
		t.Errorf("absent reasoning should stay nil, got %+v", chat.ReasoningEffort)
	}
}

func TestResponsesToolOutputImageBecomesToolMessage(t *testing.T) {
	req := api.OpenAIResponsesRequest{
		Model: "deepseek/deepseek-v4-flash",
		Input: []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "look"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "screenshot", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": []any{
				map[string]any{"type": "input_image", "image_url": fakePNG(300 * 1024)},
			}},
		},
	}

	msgs := responsesToChatRequest(req).Messages
	var tool *api.OpenAIMessage
	for i := range msgs {
		if msgs[i].Role == "tool" {
			tool = &msgs[i]
		}
	}
	if tool == nil {
		t.Fatalf("expected a tool message, got %d messages", len(msgs))
	}

	ccMsgs := ConvertMessages(msgs)
	if n := len(partOfType(ccMsgs, "image")); n != 1 {
		t.Errorf("responses tool image should be hoisted, got %d image parts", n)
	}
	for _, m := range ccMsgs {
		for _, p := range m.Content {
			if p.Output != nil && strings.Contains(p.Output.Value, "data:image") {
				t.Errorf("base64 leaked into tool-result: %q", p.Output.Value)
			}
		}
	}
}
