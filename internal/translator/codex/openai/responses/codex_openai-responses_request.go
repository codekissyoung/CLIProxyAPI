package responses

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	inputResult := util.GetGJSONBytesNoCopy(rawJSON, "input")
	if inputResult.Type == gjson.String {
		input, _ := sjson.SetBytes([]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`), "0.content.0.text", inputResult.String())
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "input", input)
		inputResult = util.GetGJSONBytesNoCopy(rawJSON, "input")
	}

	rawJSON = setCodexRequiredBool(rawJSON, "stream", true)
	rawJSON = setCodexRequiredBool(rawJSON, "store", false)
	rawJSON = setCodexRequiredBool(rawJSON, "parallel_tool_calls", true)
	rawJSON = setCodexRequiredInclude(rawJSON)

	rawJSON = SanitizeCodexResponsesRequest(rawJSON)

	// Convert role "system" to "developer" in input array to comply with Codex API requirements.
	// Re-parse input from the sanitized bytes: SanitizeCodexResponsesRequest may have
	// rewritten input items (e.g. prompt_cache_breakpoint stripping), so the earlier
	// inputResult parse is stale.
	rawJSON = convertSystemRoleToDeveloper(rawJSON)
	rawJSON = normalizeCodexBuiltinTools(rawJSON)

	return rawJSON
}

// SanitizeCodexResponsesRequest drops or rewrites top-level request fields that the
// Codex upstream rejects with {"detail":"Unsupported parameter: ..."}. It is shared
// by every translator path that forwards to Codex (OpenAI Responses, Interactions)
// so unsupported client parameters never reach the upstream account.
//
// Every sanitized request is logged at warn level (field names only, never values)
// so stripped parameters stay observable in Loki instead of disappearing silently.
func SanitizeCodexResponsesRequest(rawJSON []byte) []byte {
	var dropped []string
	for _, path := range []string{
		"max_output_tokens", "max_completion_tokens",
		"temperature", "top_p", "presence_penalty", "frequency_penalty",
		"truncation", "context_management", "metadata", "user",
		"reasoning_effort",
	} {
		if gjson.GetBytes(rawJSON, path).Exists() {
			dropped = append(dropped, path)
		}
	}
	if serviceTier := gjson.GetBytes(rawJSON, "service_tier"); serviceTier.Exists() && serviceTier.String() != "priority" {
		dropped = append(dropped, "service_tier")
	}
	responseFormat := ""
	if rf := gjson.GetBytes(rawJSON, "response_format"); rf.Exists() {
		responseFormat = "dropped"
		if !gjson.GetBytes(rawJSON, "text.format.type").Exists() {
			switch rf.Get("type").String() {
			case "text", "json_schema":
				responseFormat = "mapped_to_text_format"
			}
		}
	}

	// Codex Responses rejects token limit and sampling fields, so strip them out before forwarding.
	rawJSON = deleteCodexRequestFields(rawJSON, "max_output_tokens", "max_completion_tokens", "temperature", "top_p", "presence_penalty", "frequency_penalty")
	if serviceTier := gjson.GetBytes(rawJSON, "service_tier"); serviceTier.Exists() && serviceTier.String() != "priority" {
		rawJSON = deleteCodexRequestFields(rawJSON, "service_tier")
	}

	rawJSON = deleteCodexRequestFields(rawJSON, "truncation", "prompt_cache_options", "prompt_cache_retention")
	rawJSON = stripCodexResponsesCacheBreakpoints(rawJSON)
	rawJSON = applyResponsesCompactionCompatibility(rawJSON)

	// Codex Responses rejects chat-style response_format and metadata; map
	// response_format to text.format when possible and drop both.
	rawJSON = mapResponsesResponseFormatToTextFormat(rawJSON)
	rawJSON = deleteCodexRequestFields(rawJSON, "metadata")

	// Delete the user field as it is not supported by the Codex upstream.
	rawJSON = deleteCodexRequestFields(rawJSON, "user")

	// Codex Responses rejects the chat-style reasoning_effort field (reasoning
	// effort belongs in the reasoning object).
	rawJSON = deleteCodexRequestFields(rawJSON, "reasoning_effort")

	// Codex Responses rejects a status field on input items (clients echoing
	// back output items often carry it), so strip it from every input element.
	var strippedInputStatus bool
	rawJSON, strippedInputStatus = stripCodexInputItemStatus(rawJSON)
	if strippedInputStatus {
		dropped = append(dropped, "input[].status")
	}

	if len(dropped) > 0 || responseFormat != "" {
		fields := log.Fields{}
		if len(dropped) > 0 {
			fields["dropped_fields"] = strings.Join(dropped, ",")
		}
		if responseFormat != "" {
			fields["response_format"] = responseFormat
		}
		log.WithFields(fields).Warn("codex request sanitized: unsupported client parameters stripped before forwarding")
	}

	// Convert role "system" to "developer" in input array to comply with Codex API requirements.
	rawJSON = convertSystemRoleToDeveloper(rawJSON)
	rawJSON = normalizeCodexBuiltinTools(rawJSON)

	return rawJSON
}

func setCodexRequiredBool(rawJSON []byte, path string, value bool) []byte {
	current := gjson.GetBytes(rawJSON, path)
	if value && current.Type == gjson.True || !value && current.Type == gjson.False {
		return rawJSON
	}

	updated, errSet := sjson.SetBytes(rawJSON, path, value)
	if errSet != nil {
		return rawJSON
	}
	return updated
}

func setCodexRequiredInclude(rawJSON []byte) []byte {
	current := gjson.GetBytes(rawJSON, "include")
	values := current.Array()
	if current.IsArray() && len(values) == 1 && values[0].Type == gjson.String && values[0].String() == "reasoning.encrypted_content" {
		return rawJSON
	}

	updated, errSet := sjson.SetRawBytes(rawJSON, "include", []byte(`["reasoning.encrypted_content"]`))
	if errSet != nil {
		return rawJSON
	}
	return updated
}

func deleteCodexRequestFields(rawJSON []byte, paths ...string) []byte {
	for _, path := range paths {
		if !gjson.GetBytes(rawJSON, path).Exists() {
			continue
		}

		updated, errDelete := sjson.DeleteBytes(rawJSON, path)
		if errDelete == nil {
			rawJSON = updated
		}
	}
	return rawJSON
}

// stripCodexInputItemStatus removes the status field from every element of the
// input array. The Codex upstream answers {"detail":"Unknown parameter:
// 'input[N].status'"} when clients echo back output items that carry a status.
// Deleting object keys never reindexes the array, so original indices stay valid.
func stripCodexInputItemStatus(rawJSON []byte) (out []byte, stripped bool) {
	out = rawJSON
	input := gjson.GetBytes(out, "input")
	if !input.IsArray() {
		return out, false
	}
	for i := range input.Array() {
		path := "input." + strconv.Itoa(i) + ".status"
		if !gjson.GetBytes(out, path).Exists() {
			continue
		}
		if updated, errDelete := sjson.DeleteBytes(out, path); errDelete == nil {
			out = updated
			stripped = true
		}
	}
	return out, stripped
}

// stripCodexResponsesCacheBreakpoints removes any "prompt_cache_breakpoint" hint
// attached to individual input[].content[] items. Some clients (e.g. GitHub
// Copilot CLI) attach this field per content item when targeting the OpenAI
// Responses format. Codex Responses rejects it outright:
// {"error":{"message":"prompt_cache_breakpoint is not supported on this model", ...}}.
// The top-level prompt_cache_options strip above does not cover this nested case.
func stripCodexResponsesCacheBreakpoints(rawJSON []byte) []byte {
	if !bytes.Contains(rawJSON, []byte(`"prompt_cache_breakpoint"`)) {
		return rawJSON
	}

	input := util.GetGJSONBytesNoCopy(rawJSON, "input")
	if !input.IsArray() {
		return rawJSON
	}

	inputItems := input.Array()
	if len(inputItems) == 0 {
		return rawJSON
	}

	changed := false
	rebuiltInput := make([][]byte, 0, len(inputItems))
	for _, item := range inputItems {
		itemRaw := []byte(item.Raw)
		content := item.Get("content")
		if content.IsArray() {
			updatedContent, contentChanged := stripPromptCacheBreakpointFromContent(content)
			if contentChanged {
				if updatedItem, errSet := sjson.SetRawBytes(itemRaw, "content", updatedContent); errSet == nil {
					itemRaw = updatedItem
					changed = true
				}
			}
		}
		rebuiltInput = append(rebuiltInput, itemRaw)
	}
	if !changed {
		return rawJSON
	}

	updated, errSet := sjson.SetRawBytes(rawJSON, "input", translatorcommon.JoinRawArray(rebuiltInput))
	if errSet != nil {
		return rawJSON
	}
	return updated
}

// stripPromptCacheBreakpointFromContent removes "prompt_cache_breakpoint" from each
// content part that carries it and reports whether anything changed.
func stripPromptCacheBreakpointFromContent(content gjson.Result) ([]byte, bool) {
	parts := content.Array()
	hasBreakpoint := false
	for _, part := range parts {
		if part.Get("prompt_cache_breakpoint").Exists() {
			hasBreakpoint = true
			break
		}
	}
	if !hasBreakpoint {
		return nil, false
	}

	changed := false
	rebuiltParts := make([][]byte, 0, len(parts))
	for _, part := range parts {
		partRaw := []byte(part.Raw)
		if part.Get("prompt_cache_breakpoint").Exists() {
			if updated, errDelete := sjson.DeleteBytes(partRaw, "prompt_cache_breakpoint"); errDelete == nil {
				partRaw = updated
				changed = true
			}
		}
		rebuiltParts = append(rebuiltParts, partRaw)
	}
	if !changed {
		return nil, false
	}
	return translatorcommon.JoinRawArray(rebuiltParts), true
}

// applyResponsesCompactionCompatibility handles OpenAI Responses context_management.compaction
// for Codex upstream compatibility.
//
// Codex /responses currently rejects context_management with:
// {"detail":"Unsupported parameter: context_management"}.
//
// Compatibility strategy:
// 1) Remove context_management before forwarding to Codex upstream.
func applyResponsesCompactionCompatibility(rawJSON []byte) []byte {
	if !gjson.GetBytes(rawJSON, "context_management").Exists() {
		return rawJSON
	}

	rawJSON, _ = sjson.DeleteBytes(rawJSON, "context_management")
	return rawJSON
}

// mapResponsesResponseFormatToTextFormat converts a Chat Completions style
// response_format field into the Responses API text.format object, then removes
// response_format. Codex upstream rejects the raw field with
// {"detail":"Unsupported parameter: response_format"}.
//
// Only "text" and "json_schema" are mapped (mirroring the chat-completions
// translator); other types are dropped without mapping. An existing
// text.format.type is left untouched.
func mapResponsesResponseFormatToTextFormat(rawJSON []byte) []byte {
	rf := gjson.GetBytes(rawJSON, "response_format")
	if !rf.Exists() {
		return rawJSON
	}

	if !gjson.GetBytes(rawJSON, "text.format.type").Exists() {
		switch rf.Get("type").String() {
		case "text":
			rawJSON, _ = sjson.SetBytes(rawJSON, "text.format.type", "text")
		case "json_schema":
			if js := rf.Get("json_schema"); js.Exists() {
				rawJSON, _ = sjson.SetBytes(rawJSON, "text.format.type", "json_schema")
				if v := js.Get("name"); v.Exists() {
					rawJSON, _ = sjson.SetBytes(rawJSON, "text.format.name", v.Value())
				}
				if v := js.Get("strict"); v.Exists() {
					rawJSON, _ = sjson.SetBytes(rawJSON, "text.format.strict", v.Value())
				}
				if v := js.Get("schema"); v.Exists() {
					rawJSON, _ = sjson.SetRawBytes(rawJSON, "text.format.schema", []byte(v.Raw))
				}
			}
		}
	}

	updated, errDelete := sjson.DeleteBytes(rawJSON, "response_format")
	if errDelete == nil {
		rawJSON = updated
	}
	return rawJSON
}

// convertSystemRoleToDeveloper traverses the input array and converts any message items
// with role "system" to role "developer". This is necessary because Codex API does not
// accept "system" role in the input array.
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	return convertSystemRoleToDeveloperWithInput(rawJSON, util.GetGJSONBytesNoCopy(rawJSON, "input"))
}

func convertSystemRoleToDeveloperWithInput(rawJSON []byte, inputResult gjson.Result) []byte {
	if !inputResult.IsArray() {
		return rawJSON
	}

	inputItems := inputResult.Array()
	if len(inputItems) == 0 {
		return rawJSON
	}

	hasSystemRole := false
	for _, item := range inputItems {
		if item.IsObject() && item.Get("role").String() == "system" {
			hasSystemRole = true
			break
		}
	}
	if !hasSystemRole {
		return rawJSON
	}

	changed := false
	rebuiltInput := make([]json.RawMessage, 0, len(inputItems))
	for _, item := range inputItems {
		itemRaw := []byte(item.Raw)
		if item.IsObject() && item.Get("role").String() == "system" {
			updatedItem, errSetItem := sjson.SetRawBytes(itemRaw, "role", []byte(`"developer"`))
			if errSetItem != nil {
				return rawJSON
			}
			itemRaw = updatedItem
			changed = true
		}
		rebuiltInput = append(rebuiltInput, json.RawMessage(itemRaw))
	}
	if !changed {
		return rawJSON
	}

	inputRaw, errMarshalInput := json.Marshal(rebuiltInput)
	if errMarshalInput != nil {
		return rawJSON
	}
	updated, errSetInput := sjson.SetRawBytes(rawJSON, "input", inputRaw)
	if errSetInput != nil {
		return rawJSON
	}
	return updated
}

// normalizeCodexBuiltinTools rewrites legacy/preview built-in tool variants to the
// stable names expected by the current Codex upstream.
func normalizeCodexBuiltinTools(rawJSON []byte) []byte {
	result := normalizeCodexBuiltinToolArray(rawJSON, "tools")
	result = normalizeCodexBuiltinToolAtPath(result, "tool_choice.type")
	return normalizeCodexBuiltinToolArray(result, "tool_choice.tools")
}

func normalizeCodexBuiltinToolArray(rawJSON []byte, path string) []byte {
	tools := gjson.GetBytes(rawJSON, path)
	if !tools.IsArray() {
		return rawJSON
	}

	changed := false
	var toolItems [][]byte
	tools.ForEach(func(_, tool gjson.Result) bool {
		item := []byte(tool.Raw)
		currentType := tool.Get("type").String()
		normalizedType := normalizeCodexBuiltinToolType(currentType)
		if normalizedType != "" {
			updated, errSetType := sjson.SetBytes(item, "type", normalizedType)
			if errSetType == nil {
				item = updated
				changed = true
				log.Debugf("codex responses: normalized builtin tool type at %s.%d.type from %q to %q", path, len(toolItems), currentType, normalizedType)
			}
		}
		toolItems = append(toolItems, item)
		return true
	})
	if !changed {
		return rawJSON
	}

	updated, errSetTools := sjson.SetRawBytes(rawJSON, path, translatorcommon.JoinRawArray(toolItems))
	if errSetTools != nil {
		return rawJSON
	}
	return updated
}

func normalizeCodexBuiltinToolAtPath(rawJSON []byte, path string) []byte {
	currentType := gjson.GetBytes(rawJSON, path).String()
	normalizedType := normalizeCodexBuiltinToolType(currentType)
	if normalizedType == "" {
		return rawJSON
	}

	updated, err := sjson.SetBytes(rawJSON, path, normalizedType)
	if err != nil {
		return rawJSON
	}

	log.Debugf("codex responses: normalized builtin tool type at %s from %q to %q", path, currentType, normalizedType)
	return updated
}

// normalizeCodexBuiltinToolType centralizes the current known Codex Responses
// built-in tool alias compatibility. If Codex introduces more legacy aliases,
// extend this helper instead of adding path-specific rewrite logic elsewhere.
func normalizeCodexBuiltinToolType(toolType string) string {
	switch toolType {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return "web_search"
	default:
		return ""
	}
}
