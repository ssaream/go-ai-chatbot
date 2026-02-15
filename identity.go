package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const identityConflictReply = "I found a different account for that email/phone. Reply SWITCH to use it, or GUEST to continue here."

type identityCandidate struct {
	KeyType  string
	KeyValue string
}

func resolveIdentity(sb *SupabaseClient, in Inbound) (AppUser, string, error) {
	session, err := sb.GetUserSession(in.SessionID)
	if err != nil {
		return AppUser{}, "", err
	}

	var user AppUser
	if session == nil {
		user, err = sb.CreateAnonymousUser(in.SessionID, in.Channel)
		if err != nil {
			return AppUser{}, "", err
		}
		if err := sb.UpsertUserSession(in.SessionID, user.ID, in.Channel, map[string]any{}); err != nil {
			return AppUser{}, "", err
		}
		session = &UserSession{SessionID: in.SessionID, UserID: user.ID, Metadata: map[string]any{}}
	} else {
		user, err = sb.GetAppUserByID(session.UserID)
		if err != nil {
			return AppUser{}, "", err
		}
	}

	interrupt, switchedTo, err := handlePendingSwitch(sb, session, user, in)
	if err != nil {
		return AppUser{}, "", err
	}
	if interrupt != "" {
		return user, interrupt, nil
	}
	if switchedTo != "" {
		user, err = sb.GetAppUserByID(switchedTo)
		if err != nil {
			return AppUser{}, "", err
		}
		session.UserID = switchedTo
	}

	facts, _ := (&Router{}).extractFacts(context.Background(), in)
	candidates := buildIdentityCandidates(facts)
	for _, c := range candidates {
		found, err := sb.LookupIdentityKey(c.KeyType, c.KeyValue)
		if err != nil {
			return AppUser{}, "", err
		}
		if found != nil && found.UserID != "" && found.UserID != user.ID {
			meta := cloneMetadata(session.Metadata)
			meta["pending_switch_to_user_id"] = found.UserID
			meta["pending_switch_key_type"] = c.KeyType
			meta["pending_switch_key_value"] = c.KeyValue
			if err := sb.PatchUserSession(in.SessionID, map[string]any{"metadata": meta, "last_seen_at": time.Now().UTC().Format(time.RFC3339)}); err != nil {
				return AppUser{}, "", err
			}
			_ = sb.InsertEvent(user.ID, "", "identity.conflict", map[string]any{
				"key_type":        c.KeyType,
				"key_value":       c.KeyValue,
				"current_user_id": user.ID,
				"other_user_id":   found.UserID,
			})
			return user, identityConflictReply, nil
		}
		if found == nil {
			if err := sb.InsertIdentityKey(user.ID, c.KeyType, c.KeyValue, false); err != nil {
				return AppUser{}, "", err
			}
			_ = sb.InsertEvent(user.ID, "", "identity.key_added", map[string]any{"key_type": c.KeyType, "key_value": c.KeyValue})
		}
	}

	patch := map[string]any{"last_seen_at": time.Now().UTC().Format(time.RFC3339)}
	if user.Email == "" && facts["email"] != "" {
		patch["email"] = facts["email"]
		user.Email = facts["email"]
	}
	if user.Phone == "" && facts["phone"] != "" {
		patch["phone"] = facts["phone"]
		user.Phone = facts["phone"]
	}
	if user.Name == "" && facts["name"] != "" {
		patch["name"] = facts["name"]
		user.Name = facts["name"]
	}
	tier, status, confidence, primary := deriveIdentityState(user, in.SessionID)
	patch["identity_tier"] = tier
	patch["identity_status"] = status
	patch["confidence_score"] = confidence
	patch["primary_identifier"] = primary
	if err := sb.UpdateAppUser(user.ID, patch); err != nil {
		return AppUser{}, "", err
	}

	_ = sb.UpsertUserSession(in.SessionID, user.ID, in.Channel, session.Metadata)
	_ = sb.InsertEvent(user.ID, "", "identity.resolved", map[string]any{"session_id": in.SessionID, "tier": tier, "status": status})

	user.IdentityTier = tier
	user.IdentityStatus = status
	user.ConfidenceScore = confidence
	user.PrimaryIdentifier = primary
	return user, "", nil
}

func handlePendingSwitch(sb *SupabaseClient, session *UserSession, user AppUser, in Inbound) (string, string, error) {
	pendingID, _ := session.Metadata["pending_switch_to_user_id"].(string)
	if pendingID == "" {
		return "", "", nil
	}
	t := strings.TrimSpace(strings.ToLower(in.UserText))
	isConfirm := t == "switch" || t == "yes" || t == "confirm" || t == "use that account"
	isDecline := t == "guest" || t == "no" || t == "stay" || t == "continue"

	if isConfirm {
		delete(session.Metadata, "pending_switch_to_user_id")
		delete(session.Metadata, "pending_switch_key_type")
		delete(session.Metadata, "pending_switch_key_value")
		if err := sb.PatchUserSession(in.SessionID, map[string]any{
			"user_id":      pendingID,
			"metadata":     session.Metadata,
			"last_seen_at": time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return "", "", err
		}
		_ = sb.InsertEvent(pendingID, "", "identity.switch_confirmed", map[string]any{"from_user_id": user.ID, "to_user_id": pendingID})
		return "", pendingID, nil
	}
	if isDecline {
		delete(session.Metadata, "pending_switch_to_user_id")
		delete(session.Metadata, "pending_switch_key_type")
		delete(session.Metadata, "pending_switch_key_value")
		if err := sb.PatchUserSession(in.SessionID, map[string]any{
			"metadata":     session.Metadata,
			"last_seen_at": time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return "", "", err
		}
		_ = sb.InsertEvent(user.ID, "", "identity.switch_declined", map[string]any{"session_id": in.SessionID})
		return "", "", nil
	}
	return identityConflictReply, "", nil
}

func buildIdentityCandidates(facts map[string]string) []identityCandidate {
	candidates := []identityCandidate{}
	if facts["email"] != "" {
		candidates = append(candidates, identityCandidate{KeyType: "email", KeyValue: strings.ToLower(facts["email"])})
	}
	if facts["phone"] != "" {
		candidates = append(candidates, identityCandidate{KeyType: "phone", KeyValue: normalizePhone(facts["phone"])})
	}
	return candidates
}

func deriveIdentityState(user AppUser, sessionID string) (int, string, float64, string) {
	hasName := strings.TrimSpace(user.Name) != ""
	hasContact := strings.TrimSpace(user.Email) != "" || strings.TrimSpace(user.Phone) != ""
	switch {
	case hasContact:
		if user.Email != "" {
			return 2, "identified", 80, strings.ToLower(user.Email)
		}
		return 2, "identified", 80, normalizePhone(user.Phone)
	case hasName:
		return 1, "named", 50, user.Name
	default:
		return 0, "anonymous", 20, fmt.Sprintf("session:%s", sessionID)
	}
}

func cloneMetadata(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
