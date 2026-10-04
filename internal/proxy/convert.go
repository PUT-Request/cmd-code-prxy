package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/dev2k6/command-code-proxy-server/internal/api"
)

// Image handling constants, mirroring the CommandCode CLI's own wire layout.
const (
	// inlineImageMin: a data URL below this size inside a *string* stays as
	// text, so small inline images do not fragment tool output into extra
	// messages.
	inlineImageMin = 256 * 1024
	// maxToolImageURL: refuse a single data URL larger than this; it would
	// blow both the upstream context window and proxy memory.
	maxToolImageURL = 12 * 1024 * 1024
)

var (
	// dataURLRe matches base64 image data URLs embedded in free text.
	dataURLRe = regexp.MustCompile(`data:image/[a-z0-9.+-]+;base64,[A-Za-z0-9+/=]+`)
	// mimeRe extracts the mime type from a data URL prefix.
	mimeRe = regexp.MustCompile(`^data:([^;,]+)`)
)

// maxToolImageBytes is the per-request budget for tool-returned screenshots.
// Tool images are re-sent in full on every turn, so they are the main source of
// memory blowup and first-token latency. CC_MAX_TOOL_IMAGE_MB=0 disables it.
var maxToolImageBytes = func() int {
	mb, err := strconv.ParseFloat(os.Getenv("CC_MAX_TOOL_IMAGE_MB"), 64)
	if err != nil || mb <= 0 {
		return 6 * 1024 * 1024
	}
	return int(mb * 1024 * 1024)
}()

// Convert OpenAI messages to CommandCode format
func ConvertMessages(openAIMsgs []api.OpenAIMessage) []api.CCMessage {
	var ccMsgs []api.CCMessage
	toolNames := map[string]string{}

	for _, m := range openAIMsgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}

		if m.Role == "tool" {
			toolName := m.Name
			if toolName == "" {
				toolName = toolNames[m.ToolCallID]
			}
			if toolName == "" {
				toolName = "unknown"
			}

			// Tool output may carry images (screenshot tools return
			// [{type:"input_image", image_url:"data:image/png;base64,..."}]).
			// They must NOT be serialized into the tool-result text: the
			// upstream tokenizer reads base64 as prose, and one 2.76MB
			// screenshot costs ~1.92M tokens, blowing the 1M context window.
			// The CLI layout is: text stays in the tool result, images move to
			// a following user message.
			text, images := SplitToolOutput(m.Content)

			// The tool-result part is emitted even when the tool returned only
			// images: dropping it would break the tool-call/tool-result pairing
			// that the upstream validates.
			outputType := "text"
			if strings.HasPrefix(text, "Error:") {
				outputType = "error-text"
			}
			ccMsgs = append(ccMsgs, api.CCMessage{
				Role: "tool",
				Content: []api.CCContentPart{{
					Type:       "tool-result",
					ToolCallID: strPtr(m.ToolCallID),
					ToolName:   strPtr(toolName),
					Output: &api.CCToolOutput{
						Type:  outputType,
						Value: text,
					},
				}},
			})
			if len(images) > 0 {
				ccMsgs = append(ccMsgs, imageHoistMessage(images))
			}
			continue
		}

		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			contentParts := parseContent(m.Content, toolNames)
			addedTools := map[string]bool{}
			for _, part := range contentParts {
				if part.Type == "tool-call" && part.ToolCallID != nil {
					addedTools[*part.ToolCallID] = true
				}
			}
			for _, tc := range m.ToolCalls {
				if addedTools[tc.ID] {
					continue
				}
				contentParts = append(contentParts, api.CCContentPart{
					Type:       "tool-call",
					ToolCallID: strPtr(tc.ID),
					ToolName:   strPtr(tc.Function.Name),
					Input:      parseToolInput(tc.Function.Arguments),
				})
				addedTools[tc.ID] = true
			}
			ccMsgs = append(ccMsgs, api.CCMessage{
				Role:    m.Role,
				Content: prependReasoning(contentParts, m.ReasoningContent),
			})
			continue
		}

		contentParts := parseContent(m.Content, toolNames)
		if m.Role == "assistant" {
			contentParts = prependReasoning(contentParts, m.ReasoningContent)
		}
		ccMsgs = append(ccMsgs, api.CCMessage{Role: m.Role, Content: contentParts})
	}

	TrimToolImages(ccMsgs)
	return ccMsgs
}

// imageHoistMessage wraps tool-returned images in the user message the CLI
// places immediately after the tool result.
func imageHoistMessage(images []string) api.CCMessage {
	parts := []api.CCContentPart{
		{Type: "text", Text: strPtr("[image returned by tool call]")},
	}
	parts = append(parts, ImageParts(images)...)
	return api.CCMessage{Role: "user", Content: parts}
}

// prependReasoning puts a reasoning block first and drops the duplicate that
// parseContent may already have produced from a reasoning content block.
// CC validates that thinking is returned with the history and rejects the
// request otherwise. Order is reasoning, text, tool-call (matches the CLI).
func prependReasoning(parts []api.CCContentPart, reasoning *string) []api.CCContentPart {
	haveReasoning := reasoning != nil && *reasoning != ""

	out := make([]api.CCContentPart, 0, len(parts)+1)
	if haveReasoning {
		out = append(out, api.CCContentPart{Type: "reasoning", Text: strPtr(*reasoning)})
	}
	for _, p := range parts {
		if p.Type == "reasoning" {
			if haveReasoning {
				continue // already emitted from reasoning_content
			}
			out = append(out, p) // keep the first one from the content array
			haveReasoning = true
			continue
		}
		out = append(out, p)
	}
	return out
}

// ImageParts builds CC image content parts from data URLs, skipping entries the
// upstream cannot consume.
func ImageParts(urls []string) []api.CCContentPart {
	parts := make([]api.CCContentPart, 0, len(urls))
	for _, u := range urls {
		if p, ok := imagePart(u); ok {
			parts = append(parts, p)
		}
	}
	return parts
}

// imagePart converts a data URL into a CC image part:
// {type:"image", image:"data:image/png;base64,...", mimeType:"image/png"}.
// Non-data URLs are rejected because the upstream cannot fetch them.
func imagePart(url string) (api.CCContentPart, bool) {
	if !strings.HasPrefix(url, "data:") {
		return api.CCContentPart{}, false
	}
	mime := "image/png"
	if m := mimeRe.FindStringSubmatch(url); m != nil {
		mime = m[1]
	}
	return api.CCContentPart{
		Type:     "image",
		Image:    strPtr(url),
		MimeType: strPtr(mime),
	}, true
}

// ExtractInlineImages pulls sizable data-URL images out of free text and
// replaces each with an [image] placeholder.
func ExtractInlineImages(text string) (string, []string) {
	var images []string
	stripped := dataURLRe.ReplaceAllStringFunc(text, func(url string) string {
		if len(url) < inlineImageMin {
			return url
		}
		if len(url) > maxToolImageURL {
			return "[image omitted: too large]"
		}
		images = append(images, url)
		return "[image]"
	})
	return stripped, images
}

// SplitToolOutput separates a tool result's text from its images. output may be
// a string, an array of content blocks, or any other JSON value.
func SplitToolOutput(output any) (string, []string) {
	if output == nil {
		return "", nil
	}
	var texts []string
	var images []string

	pushText := func(raw string) {
		stripped, found := ExtractInlineImages(raw)
		if stripped != "" {
			texts = append(texts, stripped)
		}
		images = append(images, found...)
	}

	switch v := output.(type) {
	case string:
		pushText(v)
	case []any:
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				if s, ok := part.(string); ok {
					pushText(s)
				}
				continue
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "input_image", "image_url", "image":
				url := imageURLOf(pm)
				switch {
				case strings.HasPrefix(url, "data:"):
					if len(url) <= maxToolImageURL {
						images = append(images, url)
					} else {
						texts = append(texts, "[image omitted: too large]")
					}
				case url != "":
					// Remote URL: upstream cannot fetch it, keep a reference.
					texts = append(texts, "[image: "+url+"]")
				}
			default:
				if t, ok := pm["text"].(string); ok {
					pushText(t)
				} else if data, err := json.Marshal(pm); err == nil {
					pushText(string(data))
				}
			}
		}
	default:
		if data, err := json.Marshal(v); err == nil {
			pushText(string(data))
		}
	}
	return strings.Join(texts, "\n"), images
}

// imageURLOf pulls the URL out of an image content block, tolerating both
// {image_url:"..."} and {image_url:{url:"..."}} shapes.
func imageURLOf(part map[string]any) string {
	switch v := part["image_url"].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["url"].(string); ok {
			return s
		}
	}
	if s, ok := part["url"].(string); ok {
		return s
	}
	return ""
}

// TrimToolImages enforces the per-request tool-screenshot budget, keeping the
// newest images and replacing dropped ones with a placeholder so the model
// knows a screenshot was omitted rather than never existing.
func TrimToolImages(msgs []api.CCMessage) {
	if maxToolImageBytes <= 0 {
		return
	}
	type ref struct {
		msgIdx  int
		partIdx int
		size    int
	}
	var refs []ref
	for mi, m := range msgs {
		for pi, part := range m.Content {
			if part.Type == "image" && part.Image != nil {
				refs = append(refs, ref{msgIdx: mi, partIdx: pi, size: len(*part.Image)})
			}
		}
	}
	if len(refs) == 0 {
		return
	}

	keep := make(map[int]bool, len(refs))
	used := 0
	for i := len(refs) - 1; i >= 0; i-- { // newest first, always keep at least one
		if len(keep) == 0 || used+refs[i].size <= maxToolImageBytes {
			keep[i] = true
			used += refs[i].size
		}
	}
	if len(keep) == len(refs) {
		return
	}

	kept, dropped := 0, 0
	for i, r := range refs {
		if keep[i] {
			kept++
			continue
		}
		dropped++
		msgs[r.msgIdx].Content[r.partIdx] = api.CCContentPart{
			Type: "text",
			Text: strPtr("[older tool screenshot omitted: image budget exceeded]"),
		}
	}
	log.Printf("[WARN] Tool images trimmed to budget: kept=%d dropped=%d keptBytes=%d budgetBytes=%d",
		kept, dropped, used, maxToolImageBytes)
}

func ConvertTools(openAITools []any) []any {
	if len(openAITools) == 0 {
		return []any{}
	}

	tools := make([]any, 0, len(openAITools))
	for _, tool := range openAITools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			continue
		}

		toolType, _ := toolMap["type"].(string)
		if toolType != "function" {
			tools = append(tools, toolMap)
			continue
		}

		fn, ok := toolMap["function"].(map[string]any)
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}

		inputSchema, ok := fn["parameters"].(map[string]any)
		if !ok || inputSchema == nil {
			inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}

		ccTool := map[string]any{"name": name, "input_schema": inputSchema}
		if description, ok := fn["description"].(string); ok && description != "" {
			ccTool["description"] = description
		}
		tools = append(tools, ccTool)
	}

	return tools
}

func parseToolInput(arguments string) any {
	if arguments == "" {
		return map[string]any{}
	}
	var input any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		return map[string]any{"arguments": arguments}
	}
	return input
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func contentToString(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if partMap, ok := part.(map[string]any); ok {
				text := contentPartToString(partMap)
				if text != "" {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

// contentPartToString renders a content block as plain text. Image blocks are
// never rendered with their base64 payload — that would cost ~1.9M tokens per
// screenshot — so they collapse to a short reference instead.
func contentPartToString(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				b.WriteString(contentPartToString(m))
			}
		}
		return b.String()
	case map[string]any:
		if typ, _ := v["type"].(string); typ == "image_url" || typ == "input_image" || typ == "image" {
			return imageRef(imageURLOf(v))
		}
		for _, key := range []string{"text", "content", "output_text", "input_text", "refusal", "thinking", "redacted_thinking"} {
			if text, ok := v[key].(string); ok {
				return text
			}
		}
		if url := imageURLOf(v); url != "" {
			return imageRef(url)
		}
		data, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(data)
	default:
		return fmt.Sprint(v)
	}
}

// imageRef describes an image without leaking its base64 payload into text.
func imageRef(url string) string {
	if url == "" {
		return "[image]"
	}
	if strings.HasPrefix(url, "data:") {
		return "[image]"
	}
	return "[image: " + url + "]"
}

func parseContent(content interface{}, toolNames map[string]string) []api.CCContentPart {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return []api.CCContentPart{{Type: "text", Text: strPtr(v)}}
	case []any:
		var parts []api.CCContentPart
		for _, part := range v {
			partMap, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := partMap["type"].(string)
			switch typ {
			case "text", "input_text", "output_text", "refusal", "thinking", "redacted_thinking", "document", "search_result":
				if text := contentPartToString(partMap); text != "" {
					parts = append(parts, api.CCContentPart{Type: "text", Text: strPtr(text)})
				}
			case "reasoning":
				// Kept as a real reasoning block so CC can validate the thinking
				// round-trip; prependReasoning de-duplicates and reorders it.
				if text := contentPartToString(partMap); text != "" {
					parts = append(parts, api.CCContentPart{Type: "reasoning", Text: strPtr(text)})
				}
			case "image_url", "input_image", "image":
				if p, ok := imagePart(imageURLOf(partMap)); ok {
					parts = append(parts, p)
				} else if text := contentPartToString(partMap); text != "" {
					// http(s) URL: upstream cannot fetch it, keep a reference.
					parts = append(parts, api.CCContentPart{Type: "text", Text: strPtr(text)})
				}
			case "tool_use", "tool-call":
				id, _ := partMap["id"].(string)
				if id == "" {
					id, _ = partMap["toolCallId"].(string)
				}
				if id == "" {
					id, _ = partMap["tool_use_id"].(string)
				}
				name, _ := partMap["name"].(string)
				if name == "" {
					name, _ = partMap["toolName"].(string)
				}
				if id != "" && name != "" {
					toolNames[id] = name
				}
				input := partMap["input"]
				if input == nil {
					input = partMap["arguments"]
				}
				parts = append(parts, api.CCContentPart{
					Type:       "tool-call",
					ToolCallID: strPtr(id),
					ToolName:   strPtr(name),
					Input:      input,
				})
			case "tool_result", "tool-result":
				toolID, _ := partMap["tool_use_id"].(string)
				if toolID == "" {
					toolID, _ = partMap["toolCallId"].(string)
				}
				toolName, _ := partMap["toolName"].(string)
				if toolName == "" {
					toolName = toolNames[toolID]
				}
				if toolName == "" {
					toolName = "unknown"
				}
				// A tool result can carry images; hoist them out of the result
				// text so the base64 is never tokenized as prose.
				text, hoisted := SplitToolOutput(partMap["output"])
				if text == "" {
					text = SplitTextOnly(partMap["content"])
				}
				if text == "" {
					var more []string
					text, more = SplitToolOutput(partMap["content"])
					hoisted = append(hoisted, more...)
				}
				// The tool-result block is always emitted, even for an
				// image-only result, to keep the tool_call_id linkage intact.
				outputType := "text"
				if strings.HasPrefix(text, "Error:") {
					outputType = "error-text"
				}
				parts = append(parts, api.CCContentPart{
					Type:       "tool-result",
					ToolCallID: strPtr(toolID),
					ToolName:   strPtr(toolName),
					Output: &api.CCToolOutput{
						Type:  outputType,
						Value: text,
					},
				})
				if len(hoisted) > 0 {
					parts = append(parts, api.CCContentPart{
						Type: "text", Text: strPtr("[image returned by tool call]"),
					})
					parts = append(parts, ImageParts(hoisted)...)
				}
			}
		}
		return parts
	default:
		return []api.CCContentPart{{Type: "text", Text: strPtr(contentToString(v))}}
	}
}

// SplitTextOnly returns the text of a tool result, discarding any images.
func SplitTextOnly(content any) string {
	text, _ := SplitToolOutput(content)
	return text
}

// Extract system message and remaining messages
func ExtractSystem(msgs []api.OpenAIMessage) (string, []api.OpenAIMessage) {
	var system strings.Builder
	var rest []api.OpenAIMessage
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if system.Len() > 0 {
				system.WriteString("\n")
			}
			system.WriteString(contentToString(m.Content))
		} else {
			rest = append(rest, m)
		}
	}
	return system.String(), rest
}
