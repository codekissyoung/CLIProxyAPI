package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexUserAgent             = "codex-tui/0.155.1 (Mac OS 26.5.2; arm64) iTerm.app/3.7.1beta1 (codex-tui; 0.155.1)"
	codexOriginator            = "codex-tui"
	codexVersion               = "0.155.1"
	codexBetaFeatures          = "remote_compaction_v2"
	codexDefaultImageToolModel = "gpt-image-2"
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
)

// codexFallbackUserAgent intentionally converges every OAuth account on one
// captured Codex CLI persona. Per-account variation here would contradict the
// stable 0.155.1 transport profile used on the wire.
func codexFallbackUserAgent(_ *cliproxyauth.Auth) string {
	return codexUserAgent
}

var dataTag = []byte("data:")

var codexTransportCache sync.Map

const (
	codexMaxIdleConnsPerHost = 4
	codexMaxIdleConns        = 8
)

type codexCachedTransport struct {
	rt   http.RoundTripper
	base *http.Transport
}

// codexHTTPClient returns a connection pool dedicated to one auth and its
// effective proxy. This preserves a single-client upstream transport shape.
//
// ice divergence: per-auth+proxy transport caching is a deliberate local choice
// (see AGENTS.md "Forwarding model"); upstream builds one-off clients.
func codexHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) *http.Client {
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		return &http.Client{Transport: rt}
	}
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return &http.Client{Transport: buildCodexTransport(effectiveCodexProxyURL(cfg, auth)).rt}
	}

	proxyURL := effectiveCodexProxyURL(cfg, auth)
	key := auth.ID + "|" + proxyURL
	if cached, ok := codexTransportCache.Load(key); ok {
		return &http.Client{Transport: cached.(*codexCachedTransport).rt}
	}

	transport := buildCodexTransport(proxyURL)
	actual, loaded := codexTransportCache.LoadOrStore(key, transport)
	if !loaded {
		retireStaleCodexTransports(auth.ID, key)
	}
	return &http.Client{Transport: actual.(*codexCachedTransport).rt}
}

func retireStaleCodexTransports(authID, keepKey string) {
	prefix := authID + "|"
	codexTransportCache.Range(func(keyValue, _ any) bool {
		key, ok := keyValue.(string)
		if !ok || key == keepKey || !strings.HasPrefix(key, prefix) {
			return true
		}
		if old, deleted := codexTransportCache.LoadAndDelete(key); deleted {
			if cached, okCached := old.(*codexCachedTransport); okCached && cached != nil {
				if closer, okCloser := cached.rt.(interface{ CloseIdleConnections() }); okCloser {
					closer.CloseIdleConnections()
				} else if cached.base != nil {
					cached.base.CloseIdleConnections()
				}
			}
		}
		return true
	})
}

// EvictCodexTransportsForAuthID closes all cached idle connections for an auth.
func EvictCodexTransportsForAuthID(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	retireStaleCodexTransports(authID, "")
}

func effectiveCodexProxyURL(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func buildCodexTransport(proxyURL string) *codexCachedTransport {
	var transport *http.Transport
	if proxyURL == "" {
		if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok && defaultTransport != nil {
			transport = defaultTransport.Clone()
			transport.DialContext = proxyutil.IPv4OnlyDialContext
		} else {
			transport = &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: proxyutil.IPv4OnlyDialContext}
		}
	} else {
		built, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
		if errBuild != nil || built == nil {
			if errBuild != nil {
				log.Debugf("codex: invalid proxy %q, falling back to direct: %v", proxyURL, errBuild)
			}
			transport = proxyutil.NewDirectTransport()
		} else {
			transport = built
		}
	}
	tuneCodexTransport(transport)
	return &codexCachedTransport{rt: helps.NewUtlsRoundTripper(proxyURL, transport), base: transport}
}

func tuneCodexTransport(transport *http.Transport) {
	if transport == nil {
		return
	}
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConnsPerHost = codexMaxIdleConnsPerHost
	if transport.MaxIdleConns == 0 || transport.MaxIdleConns > codexMaxIdleConns {
		transport.MaxIdleConns = codexMaxIdleConns
	}
	if transport.IdleConnTimeout == 0 {
		transport.IdleConnTimeout = 90 * time.Second
	}
	if transport.TLSHandshakeTimeout == 0 {
		transport.TLSHandshakeTimeout = 10 * time.Second
	}
	if transport.ExpectContinueTimeout == 0 {
		transport.ExpectContinueTimeout = time.Second
	}
}

func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte) {
	original, body, _ := translateCodexRequestPairWithUpdateIntent(from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
	return original, body
}

func translateCodexRequestPairWithUpdateIntent(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool) {
	isCompat := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	ctx := context.Background()
	translate := func(raw []byte) ([]byte, bool) {
		if isCompat && from == sdktranslator.FormatClaude && to == sdktranslator.FormatCodex {
			return helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, nil, nil, from, to, model, raw, stream, true), false
		}
		translated := sdktranslator.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{Format: from, Model: model, Stream: stream, Body: raw})
		return translated.Body, translated.ConfigurationUpdatesChanged
	}
	if bytes.Equal(originalPayload, payload) {
		body, changed := translate(payload)
		return body, body, changed
	}
	originalTranslated, _ := translate(originalPayload)
	body, changed := translate(payload)
	return originalTranslated, body, changed
}

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Del("Authorization")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := codexHTTPClient(ctx, e.cfg, auth)
	return httpClient.Do(httpReq)
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte, headerSets ...http.Header) (*http.Request, []byte, error) {
	var headers http.Header
	if len(headerSets) > 0 {
		headers = headerSets[0]
	}
	var cache helps.CodexCache
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
		if modelName == "" {
			modelName = thinking.ParseSuffix(req.Model).ModelName
		}
		cached, ok, errCache := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, headers)
		if errCache != nil {
			return nil, nil, errCache
		}
		if ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = strings.TrimSpace(promptCacheKey.String())
		}
		if cache.ID == "" {
			cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
		}
		if cache.ID == "" {
			if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
				cache.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
			}
		}
	}
	if cache.ID == "" {
		cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
	}

	if cache.ID != "" {
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", cache.ID)
	}
	rawJSON = helps.SanitizeCodexInputItemIDs(rawJSON)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, nil, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Session-Id", cache.ID)
	}
	return httpReq, rawJSON, nil
}

// stripCodexTurnMetadataWorkspaces removes local workspace paths and git
// metadata before forwarding the otherwise opaque turn metadata header.
func stripCodexTurnMetadataWorkspaces(headers http.Header) {
	if headers == nil {
		return
	}
	originalKey := ""
	raw := ""
	for key, values := range headers {
		if !strings.EqualFold(key, "X-Codex-Turn-Metadata") {
			continue
		}
		if len(values) > 0 {
			originalKey = key
			raw = values[0]
		}
		break
	}
	if raw == "" || !gjson.Valid(raw) || !gjson.Get(raw, "workspaces").Exists() {
		return
	}
	cleaned, errDelete := sjson.Delete(raw, "workspaces")
	if errDelete != nil {
		return
	}
	headers[originalKey] = []string{cleaned}
}

// stripCodexBodyTurnMetadataWorkspaces mirrors stripCodexTurnMetadataWorkspaces
// for the body-carried copy: Codex clients mirror the WS-only turn metadata
// header into client_metadata, and the workspaces subtree (local paths, git
// remotes, commit hashes) must never leave the proxy. Unlike identity
// confusion this strip is unconditional.
func stripCodexBodyTurnMetadataWorkspaces(rawJSON []byte) []byte {
	const basePath = "client_metadata.x-codex-turn-metadata"
	turnMetadata := gjson.GetBytes(rawJSON, basePath)
	if !turnMetadata.Exists() {
		return rawJSON
	}
	if turnMetadata.IsObject() {
		if !gjson.GetBytes(rawJSON, basePath+".workspaces").Exists() {
			return rawJSON
		}
		if cleaned, errDelete := sjson.DeleteBytes(rawJSON, basePath+".workspaces"); errDelete == nil {
			return cleaned
		}
		return rawJSON
	}
	if turnMetadata.Type != gjson.String {
		return rawJSON
	}
	raw := strings.TrimSpace(turnMetadata.String())
	if raw == "" || !gjson.Valid(raw) || !gjson.Get(raw, "workspaces").Exists() {
		return rawJSON
	}
	cleaned, errDelete := sjson.Delete(raw, "workspaces")
	if errDelete != nil {
		return rawJSON
	}
	if updated, errSet := sjson.SetBytes(rawJSON, basePath, cleaned); errSet == nil {
		return updated
	}
	return rawJSON
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0]
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}
	applyCodexHeadersFromSources(r, auth, token, stream, cfg, ginHeaders)
}

// applyModelHeaderOverrides forces models.json config.override_header onto upstream headers.
func applyModelHeaderOverrides(headers http.Header, modelName string) {
	if headers == nil {
		return
	}
	overrides := registry.ModelOverrideHeaders(modelName)
	if len(overrides) == 0 {
		return
	}
	for key, value := range overrides {
		headers.Set(key, value)
	}
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") && codexSessionHeaderValue(headers) == "" {
		headers.Set("Session_id", uuid.NewString())
	}
}

// applyCodexDirectImageHeaders sets Codex upstream headers for direct /images/* calls.
// Downstream client User-Agent values are not forwarded to reduce Cloudflare 1010 blocks.
func applyCodexDirectImageHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0].Clone()
		ginHeaders.Del("User-Agent")
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header.Clone()
		ginHeaders.Del("User-Agent")
		ginHeaders.Del("Originator")
	}
	applyCodexHeadersFromSources(r, auth, token, stream, cfg, ginHeaders)
	if r.Header.Get("User-Agent") == codexUserAgent && strings.TrimSpace(r.Header.Get("Originator")) == "" {
		r.Header.Set("Originator", codexOriginator)
	}
}

func applyCodexHeadersFromSources(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, ginHeaders http.Header) {
	r.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	} else {
		r.Header.Del("Authorization")
	}

	isAPIKey := codexAuthUsesAPIKey(auth)
	cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
	if isAPIKey {
		ensureHeaderWithPriority(r.Header, ginHeaders, "X-Codex-Beta-Features", "", "")
		misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	} else {
		ensureHeaderWithPriority(r.Header, ginHeaders, "X-Codex-Beta-Features", "", codexBetaFeatures)
		misc.EnsureHeader(r.Header, ginHeaders, "Version", codexVersion)
	}
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-State", "")
	stripCodexTurnMetadataWorkspaces(r.Header)
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	if strings.TrimSpace(r.Header.Get("X-Client-Request-Id")) == "" {
		// The real CLI mints a fresh request id per request; never leave it empty.
		r.Header.Set("X-Client-Request-Id", uuid.NewString())
	}
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Window-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Thread-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Session-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, codexResponsesLiteHeader, "")
	fallbackUserAgent := codexFallbackUserAgent(auth)
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, fallbackUserAgent)

	cloakingDisabled := cfg != nil && cfg.Codex.DisableCodexCloaking
	uaForced := false
	if !cloakingDisabled && !isAPIKey && cfgUserAgent == "" {
		r.Header.Set("User-Agent", fallbackUserAgent)
		uaForced = true
	}
	if strings.TrimSpace(r.Header.Get("User-Agent")) == "" {
		r.Header.Set("User-Agent", fallbackUserAgent)
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	clientOriginator := strings.TrimSpace(ginHeaders.Get("Originator"))
	switch {
	case isAPIKey:
		if clientOriginator != "" {
			r.Header.Set("Originator", clientOriginator)
		}
	case uaForced:
		r.Header.Set("Originator", codexOriginator)
	case clientOriginator != "":
		r.Header.Set("Originator", clientOriginator)
	default:
		if !cloakingDisabled {
			r.Header.Set("Originator", codexOriginator)
		}
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
	applyCodexCloakingHeaders(r.Header, cfg, auth)
}

const codexRoutingHintHeader = "X-Codex-Routing-Hint"

// applyCodexRoutingHint sends the routing hint native Codex attaches to every
// ChatGPT-backend Responses request: "model=<slug>" plus ";tier=<service_tier>"
// when the body requests a tier (openai/codex rust-v0.155.0,
// codex-rs/core/src/client.rs build_routing_hint_header). Without it, a
// translated request carries service_tier=priority only in the body. Whether
// the backend needs the header to grant priority is undocumented.
//
// The model is the resolved model written to the upstream body, while the tier
// is read from the final body so payload rules cannot make the hint stale. A
// hint forwarded by a native client names its original model and is replaced.
// Operator configuration keeps precedence: when an auth "header:" rule for the
// hint resolves to a value (static, or a "$Header" reference the request
// carries), that value is sent, and callers apply models.json override_header
// afterwards. A rule that resolves to nothing falls back to the derived hint.
// API-key requests are not touched, matching native Codex, which sends no hint
// to API-key providers.
func applyCodexRoutingHint(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, baseModel string, upstreamBody []byte, clientHeaders http.Header) {
	if codexAuthUsesAPIKey(auth) {
		return
	}
	deleteHeaderCaseInsensitive(headers, codexRoutingHintHeader)
	if operatorHint := codexOperatorHeaderValue(ctx, auth, clientHeaders, codexRoutingHintHeader); operatorHint != "" {
		headers.Set(codexRoutingHintHeader, operatorHint)
		return
	}
	model := strings.TrimSpace(baseModel)
	if model == "" {
		return
	}
	hint := "model=" + model
	if tier := gjson.GetBytes(upstreamBody, "service_tier"); tier.Type == gjson.String {
		if value := strings.TrimSpace(tier.String()); value != "" {
			hint += ";tier=" + value
		}
	}
	headers.Set(codexRoutingHintHeader, hint)
}

// codexOperatorHeaderValue returns the value the auth's "header:" rules
// resolve to for name, using the same resolver that applied them to the
// request, so dynamic references that resolve to nothing report "".
func codexOperatorHeaderValue(ctx context.Context, auth *cliproxyauth.Auth, clientHeaders http.Header, name string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	resolved := (&http.Request{Header: http.Header{}}).WithContext(ctx)
	util.ApplyCustomHeadersFromAttrs(resolved, auth.Attributes, clientHeaders)
	return strings.TrimSpace(resolved.Header.Get(name))
}

// isCodexCloakingDisabled resolves the cloaking switch per credential, then
// per key entry, then globally (upstream semantics).
func isCodexCloakingDisabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.AuthKind() == cliproxyauth.AuthKindAPIKey {
		cfg = cfg.ForAPIKey()
	}
	if auth != nil && len(auth.Attributes) > 0 {
		if val, ok := auth.Attributes[cliproxyauth.AttributeCodexDisableCloaking]; ok {
			if parsed, errParse := strconv.ParseBool(strings.TrimSpace(val)); errParse == nil {
				return parsed
			}
		}
	}
	if entry := resolveCodexKeyConfig(cfg, auth); entry != nil && entry.DisableCodexCloaking != nil {
		return *entry.DisableCodexCloaking
	}
	if cfg != nil && cfg.Codex.DisableCodexCloaking {
		return true
	}
	return false
}

// applyCodexCloakingHeaders forces the canonical Codex CLI identity headers
// unless cloaking is disabled for this credential/key/instance.
func applyCodexCloakingHeaders(headers http.Header, cfg *config.Config, auth *cliproxyauth.Auth) {
	if headers == nil || cfg == nil || isCodexCloakingDisabled(cfg, auth) {
		return
	}
	headers.Set("User-Agent", codexUserAgent)
	headers.Set("Originator", codexOriginator)
}

func normalizeCodexInstructions(body []byte, nativeRequest ...bool) []byte {
	if len(nativeRequest) > 0 && nativeRequest[0] {
		return body
	}
	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	return body
}

var imageGenToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func isImageGenerationFunctionTool(tool gjson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("name").String() == "image_gen.imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		tools := tool.Get("tools")
		if !tools.IsArray() {
			return false
		}
		for _, nestedTool := range tools.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func ensureImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	if util.IsCodexResponsesLiteRequest(body, headers) {
		return body
	}
	if strings.HasSuffix(baseModel, "spark") {
		return body
	}
	if isCodexFreePlanAuth(auth) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", imageGenToolArrayJSON)
		return body
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(t) {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tools.-1", imageGenToolJSON)
	return body
}

func normalizeCodexParallelToolCalls(body []byte, headers http.Header) []byte {
	if util.IsCodexResponsesLiteRequest(body, headers) {
		body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
		return body
	}
	return normalizeCodexParallelToolCallsForTools(body)
}

func normalizeCodexParallelToolCallsForTools(body []byte) []byte {
	if !gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return body
	}

	body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	return body
}

func publishCodexImageToolUsage(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte) {
	detail, ok := helps.ParseCodexImageToolUsage(completedData)
	if !ok {
		return
	}
	reporter.EnsurePublished(ctx)
	reporter.PublishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return model
			}
			break
		}
	}
	return codexDefaultImageToolModel
}
