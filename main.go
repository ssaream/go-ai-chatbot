package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	anonCookie       = "kandor_anon_id"
	anonCookieMaxAge = 60 * 60 * 24 * 365
)

var extractorModel = getenv("EXTRACTOR_MODEL", "gpt-4o-mini")

var uiOrigins = splitCSV(getenv("UI_ORIGINS", "http://localhost:5173,http://127.0.0.1:5173,http://localhost:3000,http://127.0.0.1:3000,file://"))

type RuntimeConfig struct {
	SupabaseURL         string `json:"supabase_url,omitempty"`
	SupabaseServiceRole string `json:"supabase_service_role,omitempty"`
	OpenAIAPIKey        string `json:"openai_api_key,omitempty"`
	PreferredModel      string `json:"preferred_model,omitempty"`
}

var (
	cfgMu sync.RWMutex
	cfg   = RuntimeConfig{PreferredModel: "gpt-5-mini"}
)

type SessionIn struct {
	SessionID string         `json:"session_id"`
	Channel   string         `json:"channel"`
	Locale    string         `json:"locale"`
	Metadata  map[string]any `json:"metadata"`
}

type ChatIn struct {
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
	Model          string `json:"model"`
}

type ConfigIn struct {
	PreferredModel string `json:"preferred_model"`
}

type TestSupabaseIn struct {
	Table  string `json:"table"`
	Limit  int    `json:"limit"`
	Select string `json:"select"`
}

type CloseConversationIn struct {
	ConversationID string `json:"conversation_id"`
}

func main() {
	// Load .env for local development (no-op if file does not exist).
	loadDotEnvFile(".env")
	loadSecretsFromEnv()
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("static")))
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/v1/config", configHandler)
	mux.HandleFunc("/v1/models", modelsHandler)
	mux.HandleFunc("/v1/test/supabase", testSupabaseHandler)
	mux.HandleFunc("/v1/session", sessionHandler)
	mux.HandleFunc("/v1/conversation/latest", latestConversationHandler)
	mux.HandleFunc("/v1/conversation/close", closeConversationHandler)
	mux.HandleFunc("/v1/chat", chatHandler)

	h := corsMiddleware(mux)
	log.Println("Listening on :8000")
	log.Fatal(http.ListenAndServe(":8000", h))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	anon := getOrSetAnonID(w, r)
	writeJSON(w, 200, map[string]any{"ok": true, "anon_id": anon})
}

func configHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	var in ConfigIn
	_ = json.NewDecoder(r.Body).Decode(&in)
	if strings.TrimSpace(in.PreferredModel) != "" {
		cfgMu.Lock()
		cfg.PreferredModel = strings.TrimSpace(in.PreferredModel)
		cfgMu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "config": maskConfig(getConfig())})
}

func modelsHandler(w http.ResponseWriter, r *http.Request) {
	key, err := requireOpenAIKey()
	if err != nil {
		writeJSON(w, 400, map[string]any{"detail": err.Error()})
		return
	}
	req, _ := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, 502, map[string]any{"detail": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeJSON(w, 502, map[string]any{"openai_status": resp.StatusCode, "body": string(body)})
		return
	}
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	ids := []string{}
	if arr, ok := parsed["data"].([]any); ok {
		for _, v := range arr {
			if m, ok := v.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
	}
	sort.Strings(ids)
	writeJSON(w, 200, map[string]any{"models": ids, "default": "gpt-5-mini"})
}

func testSupabaseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	var in TestSupabaseIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		in = TestSupabaseIn{Table: "app_users", Limit: 1, Select: "*"}
	}
	if in.Table == "" {
		in.Table = "app_users"
	}
	if in.Limit <= 0 || in.Limit > 50 {
		in.Limit = 1
	}
	if in.Select == "" {
		in.Select = "*"
	}
	base, key, err := requireSupabase()
	if err != nil {
		writeJSON(w, 400, map[string]any{"detail": err.Error()})
		return
	}
	u := fmt.Sprintf("%s/rest/v1/%s", strings.TrimRight(base, "/"), strings.TrimLeft(in.Table, "/"))
	q := url.Values{"select": []string{in.Select}, "limit": []string{strconv.Itoa(in.Limit)}}
	req, _ := http.NewRequest(http.MethodGet, u+"?"+q.Encode(), nil)
	addSBHeaders(req, key, "")
	res, body, err := doReq(req, 25*time.Second)
	if err != nil {
		writeJSON(w, 502, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if res.StatusCode >= 400 {
		writeJSON(w, 200, map[string]any{"ok": false, "supabase_status": res.StatusCode, "body": string(body)})
		return
	}
	var rows []any
	_ = json.Unmarshal(body, &rows)
	writeJSON(w, 200, map[string]any{"ok": true, "rows_count": len(rows)})
}

func sessionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	anon := getOrSetAnonID(w, r)
	var in SessionIn
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.SessionID == "" {
		in.SessionID = newUUID()
	}
	if in.Channel == "" {
		in.Channel = "web"
	}
	if in.Locale == "" {
		in.Locale = "en"
	}
	if in.Metadata == nil {
		in.Metadata = map[string]any{}
	}
	client := &http.Client{Timeout: 90 * time.Second}
	user, err := ensureAppUserForAnon(client, anon)
	if err != nil {
		writeErr(w, err)
		return
	}
	userID := asString(user["id"])
	_ = ensureUserSession(client, in.SessionID, userID, in.Channel, merge(map[string]any{"anon_id": anon}, in.Metadata))
	conversationID, err := ensureOpenConversation(client, userID, in.SessionID, in.Channel, in.Locale, merge(map[string]any{"anon_id": anon}, in.Metadata))
	if err != nil {
		writeErr(w, err)
		return
	}
	_ = sbInsertEvent(client, userID, conversationID, "session_created", "backend", map[string]any{"anon_id": anon, "session_id": in.SessionID})
	writeJSON(w, 200, map[string]any{"anon_id": anon, "session_id": in.SessionID, "user_id": userID, "conversation_id": conversationID})
}

func latestConversationHandler(w http.ResponseWriter, r *http.Request) {
	anon := getOrSetAnonID(w, r)
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = newUUID()
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	client := &http.Client{Timeout: 90 * time.Second}
	user, err := ensureAppUserForAnon(client, anon)
	if err != nil {
		writeErr(w, err)
		return
	}
	userID := asString(user["id"])
	_ = ensureUserSession(client, sessionID, userID, "web", map[string]any{"anon_id": anon})
	convID, _ := getLatestOpenConversationID(client, userID)
	if convID == "" {
		convID, err = ensureOpenConversation(client, userID, sessionID, "web", "en", map[string]any{"anon_id": anon})
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	msgs, _ := loadConversationMessages(client, convID, limit)
	_ = sbInsertEvent(client, userID, convID, "conversation_resumed", "backend", map[string]any{"anon_id": anon, "session_id": sessionID, "limit": limit})
	writeJSON(w, 200, map[string]any{"ok": true, "anon_id": anon, "session_id": sessionID, "conversation_id": convID, "messages": msgs})
}

func closeConversationHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	anon := getOrSetAnonID(w, r)
	var in CloseConversationIn
	_ = json.NewDecoder(r.Body).Decode(&in)
	client := &http.Client{Timeout: 60 * time.Second}
	user, err := ensureAppUserForAnon(client, anon)
	if err != nil {
		writeErr(w, err)
		return
	}
	userID := asString(user["id"])
	res, err := sbGet(client, "conversations", map[string]string{"select": "id,user_id,status", "id": "eq." + in.ConversationID, "limit": "1"})
	if err != nil {
		writeErr(w, err)
		return
	}
	rows := toSliceMap(res)
	if len(rows) == 0 || asString(rows[0]["user_id"]) != userID {
		writeJSON(w, 404, map[string]any{"detail": "Conversation not found for this user."})
		return
	}
	_, _ = sbPatch(client, "conversations", map[string]any{"status": "closed", "updated_at": isoNow()}, map[string]string{"id": "eq." + in.ConversationID}, "return=minimal")
	_ = sbInsertEvent(client, userID, in.ConversationID, "conversation_closed", "backend", map[string]any{"anon_id": anon})
	writeJSON(w, 200, map[string]any{"ok": true, "conversation_id": in.ConversationID, "status": "closed"})
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	var in ChatIn
	_ = json.NewDecoder(r.Body).Decode(&in)
	if strings.TrimSpace(in.Message) == "" {
		writeJSON(w, 400, map[string]any{"detail": "Message is empty."})
		return
	}
	key, err := requireOpenAIKey()
	if err != nil {
		writeJSON(w, 400, map[string]any{"detail": err.Error()})
		return
	}
	selectedModel := in.Model
	if selectedModel == "" {
		selectedModel = getConfig().PreferredModel
	}
	if selectedModel == "" {
		selectedModel = "gpt-5-mini"
	}
	anon := getOrSetAnonID(w, r)
	if in.SessionID == "" {
		in.SessionID = newUUID()
	}
	client := &http.Client{Timeout: 90 * time.Second}
	user, err := ensureAppUserForAnon(client, anon)
	if err != nil {
		writeErr(w, err)
		return
	}
	userID := asString(user["id"])
	_ = ensureUserSession(client, in.SessionID, userID, "web", map[string]any{"anon_id": anon})
	convID := in.ConversationID
	if convID == "" {
		convID, err = ensureOpenConversation(client, userID, in.SessionID, "web", "en", map[string]any{"anon_id": anon})
		if err != nil {
			writeErr(w, err)
			return
		}
	}

	t0 := time.Now()
	extracted, extErr := aiExtractFields(client, key, in.Message)
	if extracted == nil {
		extracted = extractorFallback()
	}
	_ = sbInsertToolCall(client, convID, "ai_extractor", ternary(extErr == nil, "success", "error"), map[string]any{"model": extractorModel}, map[string]any{"latency_ms": int(time.Since(t0).Milliseconds()), "extracted": extracted, "error": errToAny(extErr)})
	_ = applyExtractedFields(client, userID, extracted)

	historyResp, _ := sbGet(client, "messages", map[string]string{"select": "role,content,created_at", "conversation_id": "eq." + convID, "order": "created_at.desc", "limit": "20"})
	rows := toSliceMap(historyResp)
	reverse(rows)
	system := "You are a helpful ecommerce assistant.\nCRITICAL: Ask AT MOST ONE question per reply.\nMVP LIMITATION: You are not connected to the real order system yet. Do NOT claim you can look up orders.\nYou can collect email/phone/order id and offer to route to support.\nNever ask for card/payment details.\n"
	msgs := []map[string]any{{"role": "system", "content": system}}
	for _, row := range rows {
		role, content := asString(row["role"]), asString(row["content"])
		if (role == "user" || role == "assistant" || role == "system") && strings.TrimSpace(content) != "" {
			msgs = append(msgs, map[string]any{"role": role, "content": content})
		}
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": in.Message})

	resp, err := openAIResponses(client, key, map[string]any{"model": selectedModel, "input": msgs, "text": map[string]any{"format": map[string]any{"type": "text"}}}, 60*time.Second)
	if err != nil {
		writeErr(w, err)
		return
	}
	reply := strings.TrimSpace(responsesText(resp))
	if reply == "" {
		reply = "(No text returned.)"
	}

	_, _ = sbPost(client, "messages", map[string]any{"conversation_id": convID, "role": "user", "content": in.Message, "payload": map[string]any{"session_id": in.SessionID, "anon_id": anon, "ts": isoNow()}}, nil, "return=minimal")
	_, _ = sbPost(client, "messages", map[string]any{"conversation_id": convID, "role": "assistant", "content": reply, "payload": map[string]any{"model_used": selectedModel, "session_id": in.SessionID, "anon_id": anon, "ts": isoNow()}}, nil, "return=minimal")
	_, _ = sbPatch(client, "conversations", map[string]any{"updated_at": isoNow()}, map[string]string{"id": "eq." + convID}, "return=minimal")
	_ = sbInsertEvent(client, userID, convID, "chat_turn", "backend", map[string]any{"anon_id": anon, "session_id": in.SessionID, "model": selectedModel})

	writeJSON(w, 200, map[string]any{"anon_id": anon, "session_id": in.SessionID, "conversation_id": convID, "reply": reply, "chat_model": selectedModel, "extracted": extracted, "extractor_model": extractorModel, "extractor_error": errToAny(extErr)})
}

func aiExtractFields(client *http.Client, key, userText string) (map[string]any, error) {
	sys := "You are an information extraction engine for an ecommerce chatbot.\nExtract ONLY what the user explicitly provided. If missing, output null.\nNormalization:\n- email: lowercase\n- phone: digits only, keep leading + if present\nOrder ID must be explicit (e.g., 'order 12345', '#12345'). Otherwise null.\nAddress must be explicitly provided. Otherwise null.\nReturn JSON only that matches the schema. Do not add extra keys.\n"
	payload := map[string]any{"model": extractorModel, "input": []map[string]any{{"role": "system", "content": sys}, {"role": "user", "content": userText}}, "temperature": 0, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "extracted_fields", "schema": extractionSchema()}}}
	resp, err := openAIResponses(client, key, payload, 60*time.Second)
	if err != nil {
		return nil, err
	}
	ex := responsesFirstJSON(resp)
	if ex == nil {
		return nil, errors.New("extractor failed: no json parsed")
	}
	if v, ok := ex["email"].(string); ok && strings.TrimSpace(v) != "" {
		ex["email"] = normalizeEmail(v)
	}
	if v, ok := ex["phone"].(string); ok && strings.TrimSpace(v) != "" {
		ex["phone"] = normalizePhone(v)
	}
	return ex, nil
}

func extractionSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"name":     map[string]any{"type": []any{"string", "null"}},
			"email":    map[string]any{"type": []any{"string", "null"}},
			"phone":    map[string]any{"type": []any{"string", "null"}},
			"order_id": map[string]any{"type": []any{"string", "null"}},
			"address":  map[string]any{"type": []any{"string", "null"}},
			"address_components": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"line1":       map[string]any{"type": []any{"string", "null"}},
					"line2":       map[string]any{"type": []any{"string", "null"}},
					"city":        map[string]any{"type": []any{"string", "null"}},
					"state":       map[string]any{"type": []any{"string", "null"}},
					"postal_code": map[string]any{"type": []any{"string", "null"}},
					"country":     map[string]any{"type": []any{"string", "null"}},
				},
				"required": []string{"line1", "line2", "city", "state", "postal_code", "country"},
			},
			"intent": map[string]any{
				"type": "string",
				"enum": []string{"product_or_content", "order_support", "returns_refunds", "shipping_delivery", "account_support", "handoff_human", "other"},
			},
			"confidence":         map[string]any{"type": "integer", "minimum": 0, "maximum": 100},
			"needs_verification": map[string]any{"type": "boolean"},
			"notes":              map[string]any{"type": []any{"string", "null"}},
		},
		"required": []string{"name", "email", "phone", "order_id", "address", "address_components", "intent", "confidence", "needs_verification", "notes"},
	}
}

func applyExtractedFields(client *http.Client, userID string, extracted map[string]any) error {
	patch := map[string]any{"last_seen_at": isoNow()}
	hasAny := false
	if v := asString(extracted["name"]); v != "" {
		patch["name"] = strings.TrimSpace(v)
		hasAny = true
	}
	if v := asString(extracted["email"]); v != "" {
		patch["email"] = normalizeEmail(v)
		hasAny = true
	}
	if v := asString(extracted["phone"]); v != "" {
		patch["phone"] = normalizePhone(v)
		hasAny = true
	}
	if hasAny {
		conf := toInt(extracted["confidence"])
		if conf < 60 {
			conf = 60
		}
		patch["identity_status"] = "identified"
		patch["identity_tier"] = 1
		patch["confidence_score"] = conf
		if asString(extracted["email"]) != "" {
			patch["primary_identifier"] = asString(extracted["email"])
		} else if asString(extracted["phone"]) != "" {
			patch["primary_identifier"] = asString(extracted["phone"])
		} else {
			patch["primary_identifier"] = asString(extracted["name"])
		}
	}
	res, err := sbPatch(client, "app_users", patch, map[string]string{"id": "eq." + userID}, "return=minimal")
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return fmt.Errorf("app_users patch failed: %d", res.StatusCode)
	}
	if v := asString(extracted["email"]); v != "" {
		_ = upsertIdentityKey(client, userID, "email", normalizeEmail(v), false)
	}
	if v := asString(extracted["phone"]); v != "" {
		_ = upsertIdentityKey(client, userID, "phone", normalizePhone(v), false)
	}
	return nil
}

func upsertIdentityKey(client *http.Client, userID, keyType, keyValue string, verified bool) error {
	payload := map[string]any{"user_id": userID, "key_type": keyType, "key_value": keyValue, "verified": verified, "first_seen_at": isoNow(), "last_seen_at": isoNow(), "metadata": map[string]any{"source": "ai_extractor"}}
	res, err := sbPost(client, "identity_keys", payload, map[string]string{"on_conflict": "user_id,key_type,key_value"}, "return=minimal,resolution=merge-duplicates")
	if err == nil && (res.StatusCode == 200 || res.StatusCode == 201 || res.StatusCode == 204) {
		return nil
	}
	_, _ = sbPost(client, "identity_keys", payload, nil, "return=minimal")
	return nil
}

func ensureAppUserForAnon(client *http.Client, anonID string) (map[string]any, error) {
	payload := map[string]any{"anonymous_id": anonID, "identity_status": "anonymous", "identity_tier": 0, "confidence_score": 30, "primary_identifier": anonID, "last_seen_at": isoNow(), "profile": map[string]any{}, "external_ids": map[string]any{}}
	res, err := sbPost(client, "app_users", payload, map[string]string{"on_conflict": "anonymous_id"}, "return=representation,resolution=merge-duplicates")
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("app_users upsert failed: %d", res.StatusCode)
	}
	rows := toSliceMap(res)
	if len(rows) > 0 {
		return rows[0], nil
	}
	g, err := sbGet(client, "app_users", map[string]string{"select": "*", "anonymous_id": "eq." + anonID, "limit": "1"})
	if err != nil {
		return nil, err
	}
	rows2 := toSliceMap(g)
	if len(rows2) == 0 {
		return nil, errors.New("app_users not found after upsert")
	}
	return rows2[0], nil
}

func ensureUserSession(client *http.Client, sessionID, userID, channel string, metadata map[string]any) error {
	ins, err := sbPost(client, "user_sessions", map[string]any{"session_id": sessionID, "user_id": userID, "channel": channel, "created_at": isoNow(), "last_seen_at": isoNow(), "metadata": metadata}, nil, "return=minimal")
	if err != nil {
		return err
	}
	if ins.StatusCode == 409 {
		upd, err := sbPatch(client, "user_sessions", map[string]any{"last_seen_at": isoNow(), "metadata": metadata}, map[string]string{"session_id": "eq." + sessionID}, "return=minimal")
		if err != nil || upd.StatusCode >= 400 {
			return fmt.Errorf("user_sessions patch failed")
		}
		return nil
	}
	if ins.StatusCode >= 400 {
		return fmt.Errorf("user_sessions insert failed")
	}
	return nil
}

func getLatestOpenConversationID(client *http.Client, userID string) (string, error) {
	res, err := sbGet(client, "conversations", map[string]string{"select": "id,updated_at", "user_id": "eq." + userID, "status": "eq.open", "order": "updated_at.desc", "limit": "1"})
	if err != nil {
		return "", err
	}
	rows := toSliceMap(res)
	if len(rows) == 0 {
		return "", nil
	}
	return asString(rows[0]["id"]), nil
}

func ensureOpenConversation(client *http.Client, userID, sessionID, channel, locale string, metadata map[string]any) (string, error) {
	cid, _ := getLatestOpenConversationID(client, userID)
	if cid != "" {
		_, _ = sbPatch(client, "conversations", map[string]any{"updated_at": isoNow()}, map[string]string{"id": "eq." + cid}, "return=minimal")
		return cid, nil
	}
	convMeta := merge(map[string]any{"session_id": sessionID}, metadata)
	ins, err := sbPost(client, "conversations", map[string]any{"user_id": userID, "status": "open", "channel": channel, "locale": locale, "metadata": convMeta}, nil, "return=representation")
	if err != nil {
		return "", err
	}
	if ins.StatusCode >= 400 {
		return "", fmt.Errorf("conversations insert failed")
	}
	rows := toSliceMap(ins)
	if len(rows) == 0 {
		return "", errors.New("missing conversation id")
	}
	return asString(rows[0]["id"]), nil
}

func loadConversationMessages(client *http.Client, conversationID string, limit int) ([]map[string]any, error) {
	res, err := sbGet(client, "messages", map[string]string{"select": "role,content,created_at", "conversation_id": "eq." + conversationID, "order": "created_at.asc", "limit": strconv.Itoa(limit)})
	if err != nil {
		return nil, err
	}
	rows := toSliceMap(res)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{"role": row["role"], "content": row["content"], "created_at": row["created_at"]})
	}
	return out, nil
}

func sbInsertToolCall(client *http.Client, conversationID, toolName, status string, requestBody, responseBody map[string]any) error {
	_, err := sbPost(client, "tool_calls", map[string]any{"conversation_id": conversationID, "tool_name": toolName, "status": status, "request": requestBody, "response": responseBody}, nil, "return=minimal")
	return err
}

func sbInsertEvent(client *http.Client, userID, conversationID, eventType, source string, payload map[string]any) error {
	_, err := sbPost(client, "events", map[string]any{"user_id": userID, "conversation_id": conversationID, "event_type": eventType, "source": source, "payload": payload}, nil, "return=minimal")
	return err
}

func openAIResponses(client *http.Client, key string, payload map[string]any, timeout time.Duration) (map[string]any, error) {
	j, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(j))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	cl := *client
	cl.Timeout = timeout
	res, body, err := doReqWithClient(&cl, req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("openai error %d: %s", res.StatusCode, string(body))
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func responsesText(resp map[string]any) string {
	if s, ok := resp["output_text"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	parts := []string{}
	if out, ok := resp["output"].([]any); ok {
		for _, item := range out {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, ok := m["content"].([]any)
			if !ok {
				continue
			}
			for _, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if asString(cm["type"]) == "output_text" {
					if t := asString(cm["text"]); strings.TrimSpace(t) != "" {
						parts = append(parts, t)
					}
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func responsesFirstJSON(resp map[string]any) map[string]any {
	if s, ok := resp["output_text"].(string); ok && strings.TrimSpace(s) != "" {
		var v map[string]any
		if json.Unmarshal([]byte(s), &v) == nil {
			return v
		}
	}
	if out, ok := resp["output"].([]any); ok {
		for _, item := range out {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, ok := m["content"].([]any)
			if !ok {
				continue
			}
			for _, c := range content {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if asString(cm["type"]) != "output_text" {
					continue
				}
				t := strings.TrimSpace(asString(cm["text"]))
				if t == "" {
					continue
				}
				var v map[string]any
				if json.Unmarshal([]byte(t), &v) == nil {
					return v
				}
			}
		}
	}
	return nil
}

func sbGet(client *http.Client, path string, params map[string]string) (*http.Response, error) {
	base, key, err := requireSupabase()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/rest/v1/%s", strings.TrimRight(base, "/"), strings.TrimLeft(path, "/"))
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	req, _ := http.NewRequest(http.MethodGet, u+"?"+q.Encode(), nil)
	addSBHeaders(req, key, "")
	res, body, err := doReqWithClient(client, req)
	if err != nil {
		return nil, err
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

func sbPost(client *http.Client, path string, body any, params map[string]string, prefer string) (*http.Response, error) {
	return sbDo(client, http.MethodPost, path, body, params, prefer)
}
func sbPatch(client *http.Client, path string, body any, params map[string]string, prefer string) (*http.Response, error) {
	return sbDo(client, http.MethodPatch, path, body, params, prefer)
}
func sbDo(client *http.Client, method, path string, payload any, params map[string]string, prefer string) (*http.Response, error) {
	base, key, err := requireSupabase()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/rest/v1/%s", strings.TrimRight(base, "/"), strings.TrimLeft(path, "/"))
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	j, _ := json.Marshal(payload)
	req, _ := http.NewRequest(method, u+"?"+q.Encode(), bytes.NewReader(j))
	addSBHeaders(req, key, prefer)
	res, body, err := doReqWithClient(client, req)
	if err != nil {
		return nil, err
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

func addSBHeaders(req *http.Request, key, prefer string) {
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}
}

func requireOpenAIKey() (string, error) {
	loadSecretsFromEnv()
	k := getConfig().OpenAIAPIKey
	if k == "" {
		return "", errors.New("Missing OpenAI API key. Set OPENAI_API_KEY.")
	}
	return k, nil
}

func requireSupabase() (string, string, error) {
	loadSecretsFromEnv()
	c := getConfig()
	if strings.TrimSpace(c.SupabaseURL) == "" || strings.TrimSpace(c.SupabaseServiceRole) == "" {
		return "", "", errors.New("Missing Supabase URL or service_role key. Set SUPABASE_URL and SUPABASE_SERVICE_ROLE (or SUPABASE_SERVICE_ROLE_KEY).")
	}
	return strings.TrimRight(c.SupabaseURL, "/"), c.SupabaseServiceRole, nil
}

func loadSecretsFromEnv() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfg.SupabaseURL = strings.TrimSpace(os.Getenv("SUPABASE_URL"))
	cfg.SupabaseServiceRole = firstNonEmptyEnv("SUPABASE_SERVICE_ROLE", "SUPABASE_SERVICE_ROLE_KEY")
	cfg.OpenAIAPIKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if cfg.PreferredModel == "" {
		cfg.PreferredModel = "gpt-5-mini"
	}
}

func loadDotEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		val := strings.TrimSpace(v)
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func getConfig() RuntimeConfig { cfgMu.RLock(); defer cfgMu.RUnlock(); return cfg }

func maskConfig(c RuntimeConfig) map[string]any {
	return map[string]any{
		"preferred_model":           c.PreferredModel,
		"has_openai_key":            strings.TrimSpace(c.OpenAIAPIKey) != "",
		"has_supabase_url":          strings.TrimSpace(c.SupabaseURL) != "",
		"has_supabase_service_role": strings.TrimSpace(c.SupabaseServiceRole) != "",
	}
}

func getOrSetAnonID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(anonCookie); err == nil && c.Value != "" {
		return c.Value
	}
	id := newUUID()
	http.SetCookie(w, &http.Cookie{Name: anonCookie, Value: id, MaxAge: anonCookieMaxAge, SameSite: http.SameSiteLaxMode, Secure: false, HttpOnly: false, Path: "/"})
	return id
}

func corsMiddleware(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range uiOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "*")
			w.Header().Set("Access-Control-Allow-Headers", "*")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func doReq(req *http.Request, timeout time.Duration) (*http.Response, []byte, error) {
	client := &http.Client{Timeout: timeout}
	return doReqWithClient(client, req)
}

func doReqWithClient(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b, nil
}

func toSliceMap(res *http.Response) []map[string]any {
	body, _ := io.ReadAll(res.Body)
	var out []map[string]any
	_ = json.Unmarshal(body, &out)
	return out
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, 502, map[string]any{"detail": err.Error()})
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func extractorFallback() map[string]any {
	return map[string]any{"name": nil, "email": nil, "phone": nil, "order_id": nil, "address": nil, "address_components": map[string]any{"line1": nil, "line2": nil, "city": nil, "state": nil, "postal_code": nil, "country": nil}, "intent": "other", "confidence": 0, "needs_verification": false, "notes": "Extractor failed"}
}

func isoNow() string  { return time.Now().UTC().Format(time.RFC3339) }
func newUUID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
func splitCSV(s string) []string {
	p := strings.Split(s, ",")
	out := []string{}
	for _, v := range p {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
func getenv(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
func normalizeEmail(x string) string { return strings.ToLower(strings.TrimSpace(x)) }
func normalizePhone(x string) string {
	x = strings.TrimSpace(x)
	if strings.HasPrefix(x, "+") {
		return "+" + onlyDigits(x[1:])
	}
	return onlyDigits(x)
}
func onlyDigits(s string) string {
	b := strings.Builder{}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func toInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		i, _ := strconv.Atoi(t)
		return i
	default:
		return 0
	}
}
func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
func ternary[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
func errToAny(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}
