package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func isDuplicateKeyError(code int, out []byte) bool {
	if code != http.StatusConflict {
		return false
	}
	return strings.Contains(string(out), `"code":"23505"`)
}

type SupabaseClient struct {
	BaseURL string
	APIKey  string
}

func (s *SupabaseClient) do(method, path string, query map[string]string, prefer string, body any) ([]byte, int, error) {
	u, err := url.Parse(s.BaseURL + path)
	if err != nil {
		return nil, 0, err
	}
	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	uStr := u.String()

	var b io.Reader
	if body != nil {
		j, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		b = bytes.NewReader(j)
	}

	req, err := http.NewRequest(method, uStr, b)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("apikey", s.APIKey)
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if prefer != "" {
		req.Header.Set("Prefer", prefer)
	}

	client := &http.Client{Timeout: 25 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

type AppUser struct {
	ID                string         `json:"id"`
	AnonymousID       string         `json:"anonymous_id"`
	Name              string         `json:"name"`
	Email             string         `json:"email"`
	Phone             string         `json:"phone"`
	IdentityTier      int            `json:"identity_tier"`
	IdentityStatus    string         `json:"identity_status"`
	ConfidenceScore   float64        `json:"confidence_score"`
	PrimaryIdentifier string         `json:"primary_identifier"`
	Profile           map[string]any `json:"profile"`
	CRMContactID      string         `json:"crm_contact_id"`
	DeskContactID     string         `json:"desk_contact_id"`
}

type UserSession struct {
	SessionID string         `json:"session_id"`
	UserID    string         `json:"user_id"`
	Metadata  map[string]any `json:"metadata"`
}

type IdentityKey struct {
	UserID string `json:"user_id"`
}

type Conversation struct {
	ID         string         `json:"id"`
	UserID     string         `json:"user_id"`
	Status     string         `json:"status"`
	Summary    string         `json:"summary"`
	LastIntent string         `json:"last_intent"`
	Channel    string         `json:"channel"`
	Locale     string         `json:"locale"`
	Metadata   map[string]any `json:"metadata"`
}

type MessageRow struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (sb *SupabaseClient) UpsertUserByAnonymousID(anonymousID, channel string) (AppUser, error) {
	u, found, err := sb.GetAppUserByAnonymousID(anonymousID)
	if err != nil {
		return AppUser{}, err
	}
	if found {
		// best-effort update channel + last_seen_at (ignore errors if columns missing)
		patch := map[string]any{
			"profile":      map[string]any{"channel": channel},
			"last_seen_at": time.Now().UTC().Format(time.RFC3339),
		}
		_, _, _ = sb.do("PATCH", "/rest/v1/app_users", map[string]string{"id": "eq." + u.ID}, "", patch)
		return u, nil
	}

	body := map[string]any{
		"anonymous_id": anonymousID,
		"profile":      map[string]any{"channel": channel},
		"last_seen_at": time.Now().UTC().Format(time.RFC3339),
	}
	out, code, err := sb.do("POST", "/rest/v1/app_users", nil, "return=representation", body)
	if err != nil {
		return AppUser{}, err
	}
	if code >= 300 {
		if isDuplicateKeyError(code, out) {
			u, found, lookupErr := sb.GetAppUserByAnonymousID(anonymousID)
			if lookupErr != nil {
				return AppUser{}, lookupErr
			}
			if found {
				return u, nil
			}
		}
		return AppUser{}, fmt.Errorf("supabase insert app_users (%d): %s", code, string(out))
	}

	var users []AppUser
	_ = json.Unmarshal(out, &users)
	if len(users) == 0 {
		return AppUser{}, fmt.Errorf("insert app_users returned empty")
	}
	return users[0], nil
}

func (sb *SupabaseClient) GetAppUserByAnonymousID(anonymousID string) (AppUser, bool, error) {
	out, code, err := sb.do("GET", "/rest/v1/app_users", map[string]string{
		"anonymous_id": "eq." + anonymousID,
		"select":       "id,anonymous_id,name,email,phone,profile,crm_contact_id,desk_contact_id",
		"limit":        "1",
	}, "", nil)
	if err != nil {
		return AppUser{}, false, err
	}
	if code >= 300 {
		return AppUser{}, false, fmt.Errorf("supabase select app_users (%d): %s", code, string(out))
	}

	var users []AppUser
	_ = json.Unmarshal(out, &users)
	if len(users) > 0 {
		return users[0], true, nil
	}
	return AppUser{}, false, nil
}

func (sb *SupabaseClient) GetOrCreateOpenConversation(userID, anonymousID, channel, locale string) (Conversation, error) {
	out, code, err := sb.do("GET", "/rest/v1/conversations", map[string]string{
		"user_id": "eq." + userID,
		"status":  "eq.open",
		"select":  "id,user_id,status,summary,last_intent,channel,locale,metadata",
		"order":   "updated_at.desc",
		"limit":   "1",
	}, "", nil)
	if err != nil {
		return Conversation{}, err
	}
	if code >= 300 {
		return Conversation{}, fmt.Errorf("supabase select conversations (%d): %s", code, string(out))
	}
	var convs []Conversation
	_ = json.Unmarshal(out, &convs)
	if len(convs) > 0 {
		return convs[0], nil
	}

	body := map[string]any{
		"user_id":  userID,
		"status":   "open",
		"channel":  channel,
		"locale":   locale,
		"metadata": map[string]any{"session_id": anonymousID, "facts": map[string]any{}},
	}
	out, code, err = sb.do("POST", "/rest/v1/conversations", nil, "return=representation", body)
	if err != nil {
		return Conversation{}, err
	}
	if code >= 300 {
		return Conversation{}, fmt.Errorf("supabase insert conversations (%d): %s", code, string(out))
	}
	_ = json.Unmarshal(out, &convs)
	if len(convs) == 0 {
		return Conversation{}, fmt.Errorf("insert conversations returned empty")
	}
	return convs[0], nil
}

func (sb *SupabaseClient) GetOpenConversationByAnonymousID(anonymousID string) (Conversation, bool, error) {
	user, found, err := sb.GetAppUserByAnonymousID(anonymousID)
	if err != nil {
		return Conversation{}, false, err
	}
	if !found {
		return Conversation{}, false, nil
	}
	out, code, err := sb.do("GET", "/rest/v1/conversations", map[string]string{
		"user_id": "eq." + user.ID,
		"status":  "eq.open",
		"select":  "id,user_id,status,summary,last_intent,channel,locale,metadata",
		"order":   "updated_at.desc",
		"limit":   "1",
	}, "", nil)
	if err != nil {
		return Conversation{}, false, err
	}
	if code >= 300 {
		return Conversation{}, false, fmt.Errorf("supabase select conversations (%d): %s", code, string(out))
	}
	var convs []Conversation
	_ = json.Unmarshal(out, &convs)
	if len(convs) == 0 {
		return Conversation{}, false, nil
	}
	return convs[0], true, nil
}

func (sb *SupabaseClient) CloseOpenConversationsByAnonymousID(anonymousID string) error {
	user, found, err := sb.GetAppUserByAnonymousID(anonymousID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	patch := map[string]any{
		"status": "closed",
	}
	out, code, err := sb.do("PATCH", "/rest/v1/conversations", map[string]string{
		"user_id": "eq." + user.ID,
		"status":  "eq.open",
	}, "", patch)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase close conversations (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) Ping() error {
	_, code, err := sb.do("GET", "/rest/v1/app_users", map[string]string{
		"select": "id",
		"limit":  "1",
	}, "", nil)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase ping failed (%d)", code)
	}
	return nil
}

func (sb *SupabaseClient) FetchRecentMessages(conversationID string, limit int) ([]MessageRow, error) {
	out, code, err := sb.do("GET", "/rest/v1/messages", map[string]string{
		"conversation_id": "eq." + conversationID,
		"select":          "role,content,created_at",
		"order":           "created_at.desc",
		"limit":           strconv.Itoa(limit),
	}, "", nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fmt.Errorf("supabase select messages (%d): %s", code, string(out))
	}
	var rows []MessageRow
	_ = json.Unmarshal(out, &rows)
	// reverse
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

func (sb *SupabaseClient) InsertMessage(conversationID, role, content string, payload map[string]any) error {
	body := map[string]any{
		"conversation_id": conversationID,
		"role":            role,
		"content":         content,
		"payload":         payload,
	}
	out, code, err := sb.do("POST", "/rest/v1/messages", nil, "", body)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase insert messages (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) UpdateConversation(conversationID string, patch map[string]any) error {
	out, code, err := sb.do("PATCH", "/rest/v1/conversations", map[string]string{
		"id": "eq." + conversationID,
	}, "", patch)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase update conversations (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) UpsertIdempotency(key string) (already bool, err error) {
	// expects idempotency_keys(key text unique, created_at timestamptz default now())
	out, code, err := sb.do("GET", "/rest/v1/idempotency_keys", map[string]string{
		"key":    "eq." + key,
		"select": "key",
		"limit":  "1",
	}, "", nil)
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, fmt.Errorf("supabase select idempotency_keys (%d): %s", code, string(out))
	}
	var rows []map[string]any
	_ = json.Unmarshal(out, &rows)
	if len(rows) > 0 {
		return true, nil
	}
	_, code, err = sb.do("POST", "/rest/v1/idempotency_keys", nil, "", map[string]any{"key": key})
	if err != nil {
		return false, err
	}
	if code >= 300 {
		return false, fmt.Errorf("supabase insert idempotency_keys (%d)", code)
	}
	return false, nil
}

func (sb *SupabaseClient) GetUserSession(sessionID string) (*UserSession, error) {
	out, code, err := sb.do("GET", "/rest/v1/user_sessions", map[string]string{
		"session_id": "eq." + sessionID,
		"select":     "session_id,user_id,metadata",
		"limit":      "1",
	}, "", nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fmt.Errorf("supabase select user_sessions (%d): %s", code, string(out))
	}
	var rows []UserSession
	_ = json.Unmarshal(out, &rows)
	if len(rows) == 0 {
		return nil, nil
	}
	if rows[0].Metadata == nil {
		rows[0].Metadata = map[string]any{}
	}
	return &rows[0], nil
}

func (sb *SupabaseClient) UpsertUserSession(sessionID, userID, channel string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	body := map[string]any{
		"session_id":   sessionID,
		"user_id":      userID,
		"channel":      channel,
		"metadata":     metadata,
		"last_seen_at": time.Now().UTC().Format(time.RFC3339),
	}
	out, code, err := sb.do("POST", "/rest/v1/user_sessions", nil, "resolution=merge-duplicates", body)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase upsert user_sessions (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) PatchUserSession(sessionID string, patch map[string]any) error {
	out, code, err := sb.do("PATCH", "/rest/v1/user_sessions", map[string]string{
		"session_id": "eq." + sessionID,
	}, "", patch)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase update user_sessions (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) LookupIdentityKey(keyType, keyValue string) (*IdentityKey, error) {
	out, code, err := sb.do("GET", "/rest/v1/identity_keys", map[string]string{
		"key_type":  "eq." + keyType,
		"key_value": "eq." + keyValue,
		"select":    "user_id",
		"limit":     "1",
	}, "", nil)
	if err != nil {
		return nil, err
	}
	if code >= 300 {
		return nil, fmt.Errorf("supabase select identity_keys (%d): %s", code, string(out))
	}
	var rows []IdentityKey
	_ = json.Unmarshal(out, &rows)
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func (sb *SupabaseClient) InsertIdentityKey(userID, keyType, keyValue string, verified bool) error {
	body := map[string]any{
		"user_id":   userID,
		"key_type":  keyType,
		"key_value": keyValue,
		"verified":  verified,
	}
	out, code, err := sb.do("POST", "/rest/v1/identity_keys", nil, "resolution=ignore-duplicates", body)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase insert identity_keys (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) CreateAnonymousUser(sessionID, channel string) (AppUser, error) {
	body := map[string]any{
		"anonymous_id":       sessionID,
		"identity_tier":      0,
		"identity_status":    "anonymous",
		"confidence_score":   20,
		"primary_identifier": sessionID,
		"profile":            map[string]any{"channel": channel},
		"last_seen_at":       time.Now().UTC().Format(time.RFC3339),
	}
	out, code, err := sb.do("POST", "/rest/v1/app_users", nil, "return=representation", body)
	if err != nil {
		return AppUser{}, err
	}
	if code >= 300 {
		if isDuplicateKeyError(code, out) {
			u, found, lookupErr := sb.GetAppUserByAnonymousID(sessionID)
			if lookupErr != nil {
				return AppUser{}, lookupErr
			}
			if found {
				return u, nil
			}
		}
		return AppUser{}, fmt.Errorf("supabase insert app_users (%d): %s", code, string(out))
	}
	var users []AppUser
	_ = json.Unmarshal(out, &users)
	if len(users) == 0 {
		return AppUser{}, fmt.Errorf("insert app_users returned empty")
	}
	return users[0], nil
}

func (sb *SupabaseClient) GetAppUserByID(userID string) (AppUser, error) {
	out, code, err := sb.do("GET", "/rest/v1/app_users", map[string]string{
		"id":     "eq." + userID,
		"select": "id,anonymous_id,name,email,phone,identity_tier,identity_status,confidence_score,primary_identifier,profile,crm_contact_id,desk_contact_id",
		"limit":  "1",
	}, "", nil)
	if err != nil {
		return AppUser{}, err
	}
	if code >= 300 {
		return AppUser{}, fmt.Errorf("supabase select app_users (%d): %s", code, string(out))
	}
	var users []AppUser
	_ = json.Unmarshal(out, &users)
	if len(users) == 0 {
		return AppUser{}, fmt.Errorf("app_user not found")
	}
	return users[0], nil
}

func (sb *SupabaseClient) UpdateAppUser(userID string, patch map[string]any) error {
	out, code, err := sb.do("PATCH", "/rest/v1/app_users", map[string]string{"id": "eq." + userID}, "", patch)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase update app_users (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) InsertEvent(userID, conversationID, eventType string, payload map[string]any) error {
	body := map[string]any{
		"user_id":         userID,
		"conversation_id": nil,
		"event_type":      eventType,
		"payload":         payload,
	}
	if conversationID != "" {
		body["conversation_id"] = conversationID
	}
	out, code, err := sb.do("POST", "/rest/v1/events", nil, "", body)
	if err != nil {
		return err
	}
	if code >= 300 {
		return fmt.Errorf("supabase insert events (%d): %s", code, string(out))
	}
	return nil
}

func (sb *SupabaseClient) ResolveIdentity(ctx context.Context, in Inbound) (AppUser, string, error) {
	_ = ctx
	return resolveIdentity(sb, in)
}
