package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const sessionCookieName = "sid"

type OpenAIClient struct {
	APIKey string
	Model  string
}

// Keep this type exactly as-is since router.go already uses it.
type openAIChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

/*
Responses API request/response structs
Docs: POST /v1/responses
*/
type responsesInputMessage struct {
	Role    string `json:"role"`    // "developer" | "system" | "user" | "assistant"
	Content string `json:"content"` // plain text
}

type responsesCreateRequest struct {
	Model           string                  `json:"model"`
	Input           []responsesInputMessage `json:"input,omitempty"`
	Instructions    string                  `json:"instructions,omitempty"`
	Truncation      string                  `json:"truncation,omitempty"` // "auto"
	Text            map[string]any          `json:"text,omitempty"`
	MaxOutputTokens int                     `json:"max_output_tokens,omitempty"`
}

type responsesCreateResponse struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	// Some SDK/examples expose a convenience concatenation field.
	OutputText string `json:"output_text"`

	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`

	Output []struct {
		Type    string `json:"type"` // "message"
		Role    string `json:"role"` // "assistant"
		Text    any    `json:"text"`
		Refusal any    `json:"refusal"`
		Content []struct {
			Type    string `json:"type"` // "output_text" | "refusal"
			Text    any    `json:"text"`
			Refusal any    `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
}

// Chat keeps the SAME signature your router.go currently calls.
// Internally it uses the Responses API now.
func (o *OpenAIClient) Chat(system string, summary string, history []openAIChatMsg, userText string) (string, error) {
	model := strings.TrimSpace(o.Model)
	if model == "" {
		model = "gpt-5-mini"
	}
	maxOutputTokens := getenvIntDefault("OPENAI_MAX_OUTPUT_TOKENS", 180)
	verbosity := getenvDefault("OPENAI_VERBOSITY", "low")

	// Build Responses input messages (simple role+string content per docs)
	input := make([]responsesInputMessage, 0, 2+len(history)+1)

	// Put app instructions into `instructions` (preferred)
	instructions := strings.TrimSpace(system)

	// Add summary (if any) as additional developer context
	if strings.TrimSpace(summary) != "" {
		input = append(input, responsesInputMessage{
			Role:    "developer",
			Content: "Conversation summary: " + summary,
		})
	}

	// Add prior chat history
	for _, m := range history {
		role := m.Role
		switch role {
		case "user", "assistant", "system", "developer":
			// ok
		default:
			role = "user"
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		input = append(input, responsesInputMessage{
			Role:    role,
			Content: m.Content,
		})
	}

	// Add latest user message
	input = append(input, responsesInputMessage{
		Role:    "user",
		Content: userText,
	})

	reqBody := responsesCreateRequest{
		Model:           model,
		Input:           input,
		Instructions:    instructions,
		Truncation:      "auto",
		MaxOutputTokens: maxOutputTokens,
		// Ask for plain text output
		Text: map[string]any{
			"format":    map[string]any{"type": "text"},
			"verbosity": verbosity,
		},
	}

	replyText, err := o.createResponse(reqBody)
	if err != nil {
		return "", err
	}

	if len(replyText) > 800 {
		rewriteReq := responsesCreateRequest{
			Model:        model,
			Instructions: "Rewrite the above in <= 4 bullets, <= 450 chars, keep facts.",
			Input: []responsesInputMessage{{
				Role:    "user",
				Content: replyText,
			}},
			Truncation:      "auto",
			MaxOutputTokens: 90,
			Text: map[string]any{
				"format":    map[string]any{"type": "text"},
				"verbosity": "low",
			},
		}
		rewritten, rewriteErr := o.createResponse(rewriteReq)
		if rewriteErr == nil && strings.TrimSpace(rewritten) != "" {
			replyText = rewritten
		}
	}

	return strings.TrimSpace(replyText), nil
}

func (o *OpenAIClient) createResponse(reqBody responsesCreateRequest) (string, error) {
	return o.createResponseWithRetry(reqBody, 0)
}

func (o *OpenAIClient) createResponseWithRetry(reqBody responsesCreateRequest, attempt int) (string, error) {

	j, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("openai marshal error: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/responses", bytes.NewReader(j))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("openai responses api error (%d): %s", resp.StatusCode, string(out))
	}

	var parsed responsesCreateResponse
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", fmt.Errorf("openai parse error: %w | raw=%s", err, string(out))
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("openai error (%s): %s", parsed.Error.Code, parsed.Error.Message)
	}

	if reply := extractParsedResponseText(parsed); reply != "" {
		return reply, nil
	}

	if fallback := extractResponseTextFallback(out); fallback != "" {
		return fallback, nil
	}

	if parsed.Status == "incomplete" && attempt == 0 {
		reason := ""
		if parsed.IncompleteDetails != nil {
			reason = parsed.IncompleteDetails.Reason
		}
		if reason == "max_output_tokens" || reason == "" {
			retryReq := reqBody
			if retryReq.MaxOutputTokens <= 0 {
				retryReq.MaxOutputTokens = 180
			}
			retryReq.MaxOutputTokens = min(retryReq.MaxOutputTokens*2, 1200)
			if strings.TrimSpace(retryReq.Instructions) == "" {
				retryReq.Instructions = "Return a direct plain-text answer."
			} else {
				retryReq.Instructions += " Return a direct plain-text answer."
			}
			if retryReq.Text == nil {
				retryReq.Text = map[string]any{}
			}
			retryReq.Text["format"] = map[string]any{"type": "text"}
			retryReq.Text["verbosity"] = "low"
			return o.createResponseWithRetry(retryReq, attempt+1)
		}
		if reason == "content_filter" {
			return "I can’t help with that request as written. Please rephrase and avoid sharing sensitive personal details.", nil
		}
	}

	return "", fmt.Errorf("openai: no text found in response (status=%s)", parsed.Status)
}

func extractParsedResponseText(parsed responsesCreateResponse) string {
	if strings.TrimSpace(parsed.OutputText) != "" {
		return strings.TrimSpace(parsed.OutputText)
	}

	var reply strings.Builder
	for _, item := range parsed.Output {
		// Some Responses payloads include output items that are not "message" and may not include role.
		// Extract text from:
		//  - assistant "message" items (from content parts), and
		//  - non-message items (e.g., "output_text") that place text at the top level.
		if item.Type == "message" {
			if item.Role != "assistant" {
				continue
			}
			for _, c := range item.Content {
				if c.Type != "output_text" && c.Type != "text" && c.Type != "refusal" {
					continue
				}
				reply.WriteString(extractTextValue(c.Text))
				reply.WriteString(extractTextValue(c.Refusal))
			}
			continue
		}

		// Non-message output items: try both text and refusal.
		reply.WriteString(extractTextValue(item.Text))
		reply.WriteString(extractTextValue(item.Refusal))
	}

	return strings.TrimSpace(reply.String())
}

func extractTextValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if val, ok := t["value"].(string); ok {
			return val
		}
		if txt, ok := t["text"].(string); ok {
			return txt
		}
	}
	return ""
}

func extractResponseTextFallback(raw []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}

	if s, ok := payload["output_text"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}

	items, _ := payload["output"].([]any)
	var reply strings.Builder
	for _, itemAny := range items {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		itype, _ := item["type"].(string)
		if itype == "message" {
			if role, _ := item["role"].(string); role != "assistant" {
				continue
			}
			content, _ := item["content"].([]any)
			for _, partAny := range content {
				part, ok := partAny.(map[string]any)
				if !ok {
					continue
				}
				reply.WriteString(extractTextValue(part["text"]))
				reply.WriteString(extractTextValue(part["refusal"]))
			}
			continue
		}

		// Non-message items (e.g., output_text) often have no role.
		reply.WriteString(extractTextValue(item["text"]))
		reply.WriteString(extractTextValue(item["refusal"]))
	}

	return strings.TrimSpace(reply.String())
}

// -------- cookies --------

func newSessionID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "anon_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func cookieSecure() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("COOKIE_SECURE")))
	return v == "true" || v == "1" || v == "yes"
}

func ensureSessionCookie(w http.ResponseWriter, r *http.Request) (string, bool, error) {
	if c, err := r.Cookie(sessionCookieName); err == nil && strings.TrimSpace(c.Value) != "" {
		return c.Value, false, nil
	}
	sid, err := newSessionID()
	if err != nil {
		return "", false, err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookieSecure(),
		Expires:  time.Now().Add(90 * 24 * time.Hour),
	})
	return sid, true, nil
}

func readSessionID(r *http.Request) (string, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(c.Value) == "" {
		return "", fmt.Errorf("missing session cookie")
	}
	return c.Value, nil
}

// -------- helpers --------

func getenvDefault(k, def string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	return v
}

func getenvIntDefault(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// -------- web handlers --------

type ChatHTTPReq struct {
	Message string `json:"message"`
	Channel string `json:"channel"`
	Locale  string `json:"locale"`
	Model   string `json:"model,omitempty"`
}

type modelsListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (o *OpenAIClient) ListModels() ([]string, error) {
	req, err := http.NewRequest("GET", "https://api.openai.com/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openai models api error (%d): %s", resp.StatusCode, string(body))
	}
	var parsed modelsListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openai models parse error: %w", err)
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if strings.TrimSpace(m.ID) != "" {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func main() {
	sbURL := strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	sbKey := os.Getenv("SUPABASE_SERVICE_ROLE_KEY")
	oaKey := os.Getenv("OPENAI_API_KEY")
	oaModel := getenvDefault("OPENAI_MODEL", "gpt-5-mini")

	tools := &Tools{
		Shopify:  NoopShopify{},
		ZohoCRM:  NoopZohoCRM{},
		ZohoDesk: NoopZohoDesk{},
		Brevo:    NoopBrevo{},
		WhatsApp: NoopWhatsApp{},
	}

	routingSpecs := RoutingTable()

	// Static files with cookie initialization
	fs := http.FileServer(http.Dir("./static"))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _, err := ensureSessionCookie(w, r)
		if err != nil {
			http.Error(w, "failed to set session", 500)
			return
		}
		fs.ServeHTTP(w, r)
	})

	http.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		sid, _, err := ensureSessionCookie(w, r)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"session_id": sid})
	})

	http.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		effectiveOAKey := strings.TrimSpace(r.Header.Get("X-OpenAI-Api-Key"))
		if effectiveOAKey == "" {
			effectiveOAKey = oaKey
		}
		if effectiveOAKey == "" {
			writeJSON(w, 400, map[string]any{"error": "missing OpenAI API key"})
			return
		}
		client := &OpenAIClient{APIKey: effectiveOAKey, Model: oaModel}
		models, err := client.ListModels()
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"models": models})
	})

	http.HandleFunc("/new-session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sid, err := newSessionID()
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   cookieSecure(),
			Expires:  time.Now().Add(90 * 24 * time.Hour),
		})
		writeJSON(w, 200, map[string]any{"session_id": sid})
	})

	http.HandleFunc("/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if _, _, err := ensureSessionCookie(w, r); err != nil {
			writeJSON(w, 500, map[string]any{"error": "failed to establish session"})
			return
		}
		sid, err := readSessionID(r)
		if err != nil {
			writeJSON(w, 400, map[string]any{"error": "missing session"})
			return
		}

		var req ChatHTTPReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": "invalid json"})
			return
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			writeJSON(w, 400, map[string]any{"error": "message required"})
			return
		}
		if req.Channel == "" {
			req.Channel = "web"
		}
		if req.Locale == "" {
			req.Locale = "en-IN"
		}

		effectiveSBURL := strings.TrimRight(strings.TrimSpace(r.Header.Get("X-Supabase-Url")), "/")
		if effectiveSBURL == "" {
			effectiveSBURL = sbURL
		}

		effectiveSBKey := strings.TrimSpace(r.Header.Get("X-Supabase-Service-Role-Key"))
		if effectiveSBKey == "" {
			effectiveSBKey = sbKey
		}

		effectiveOAKey := strings.TrimSpace(r.Header.Get("X-OpenAI-Api-Key"))
		if effectiveOAKey == "" {
			effectiveOAKey = oaKey
		}

		if effectiveSBURL == "" || effectiveSBKey == "" || effectiveOAKey == "" {
			writeJSON(w, 400, map[string]any{
				"error": "missing configuration: provide Supabase URL, Supabase service role key, and OpenAI API key (headers or env vars)",
			})
			return
		}

		effectiveRouter := &Router{
			SB: &SupabaseClient{
				BaseURL: effectiveSBURL,
				APIKey:  effectiveSBKey,
			},
			LLM: &OpenAIClient{
				APIKey: effectiveOAKey,
				Model:  firstNonEmpty(strings.TrimSpace(req.Model), oaModel),
			},
			Tools: tools,
			Specs: routingSpecs,
		}

		out, err := effectiveRouter.Handle(r.Context(), Inbound{
			Channel:   req.Channel,
			Locale:    req.Locale,
			SessionID: sid,
			UserText:  req.Message,
		})
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{
			"intent": string(out.Intent),
			"reply":  out.Reply,
		})
	})

	port := getenvDefault("PORT", "8081")
	log.Printf("Server running on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
