package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
)

type Router struct {
	SB    *SupabaseClient
	LLM   *OpenAIClient
	Tools *Tools
	Specs map[Intent]IntentSpec
}

type Inbound struct {
	Channel       string // web / whatsapp_meta
	Locale        string
	SessionID     string // sid cookie OR wa:<phone>
	UserText      string
	WhatsAppMsgID string // for idempotency, optional
	WhatsAppFrom  string // phone, optional
}

type RouteResult struct {
	Intent         Intent
	Reply          string
	ConversationID string
	Extracted      map[string]string
	ExtractorError string
}

func (rt *Router) Handle(ctx context.Context, in Inbound) (RouteResult, error) {
	// 0) idempotency for WhatsApp message retries
	if in.WhatsAppMsgID != "" {
		key := "wa_msg:" + in.WhatsAppMsgID
		already, err := rt.SB.UpsertIdempotency(key)
		if err != nil {
			return RouteResult{}, err
		}
		if already {
			return RouteResult{Intent: IntentOther, Reply: "✅ Got it. (Duplicate message ignored.)", ConversationID: "", Extracted: map[string]string{}}, nil
		}
	}

	// 1) upsert user + conversation
	user, interruptReply, err := rt.SB.ResolveIdentity(ctx, in)
	if err != nil {
		return RouteResult{}, err
	}
	conv, err := rt.SB.GetOrCreateOpenConversation(user.ID, in.SessionID, in.Channel, in.Locale)
	if err != nil {
		return RouteResult{}, err
	}

	// 2) persist inbound message first (so the DB always has the user message even if downstream fails)
	_ = rt.SB.InsertMessage(conv.ID, "user", in.UserText, map[string]any{"channel": in.Channel})

	if interruptReply != "" {
		_ = rt.SB.InsertMessage(conv.ID, "assistant", interruptReply, map[string]any{"intent": "identity_interrupt"})
		return RouteResult{Intent: IntentOther, Reply: interruptReply, ConversationID: conv.ID, Extracted: map[string]string{}}, nil
	}

	// 3) load memory
	recent, err := rt.SB.FetchRecentMessages(conv.ID, 10)
	if err != nil {
		return RouteResult{}, err
	}

	// 4) extract facts (order_id/email/phone/name/item/reason)
	facts, extractorErr := rt.extractFacts(ctx, in)

	// 5) classify intent (fast heuristic; LLM can refine later)
	intent := classifyIntent(in.UserText)

	// 6) merge facts into conversation metadata so they persist across turns
	convFacts := getFactsFromMetadata(conv.Metadata)
	for k, v := range facts {
		if v != "" {
			convFacts[k] = v
		}
	}
	conv.Metadata = setFactsInMetadata(conv.Metadata, convFacts)

	spec, ok := rt.Specs[intent]
	if !ok {
		intent = IntentOther
		spec = rt.Specs[IntentOther]
	}

	// 7) check required fields
	missing := missingFields(spec, convFacts, in)
	if len(missing) > 0 {
		reply := rt.askForMissing(spec, missing)
		_ = rt.persistAssistant(conv, intent, reply, convFacts)
		return RouteResult{Intent: intent, Reply: reply, ConversationID: conv.ID, Extracted: facts, ExtractorError: extractorErr}, nil
	}

	// 8) tool plan (safe + minimal)
	reply, toolErr := rt.executeToolsIfNeeded(ctx, intent, spec, user, conv, convFacts, recent, in.UserText)
	if toolErr != nil {
		// fallback: LLM response without tools
		reply = rt.llmReply(intent, conv.Summary, recent, in.UserText, convFacts)
	}

	_ = rt.persistAssistant(conv, intent, reply, convFacts)
	return RouteResult{Intent: intent, Reply: reply, ConversationID: conv.ID, Extracted: facts, ExtractorError: extractorErr}, nil
}

func (rt *Router) executeToolsIfNeeded(
	ctx context.Context,
	intent Intent,
	spec IntentSpec,
	user AppUser,
	conv Conversation,
	facts map[string]string,
	recent []MessageRow,
	userText string,
) (string, error) {

	// Shopify lookup
	if spec.ToolPlan.NeedsShopifyLookup && rt.Tools != nil && rt.Tools.Shopify != nil {
		ord, err := rt.Tools.Shopify.LookupOrder(ctx, facts)
		if err != nil {
			return "", err
		}
		if ord == nil {
			return "I couldn’t find a matching order. Please recheck the Order ID, or share the email/phone used at checkout.", nil
		}
		msg := fmt.Sprintf("Here’s what I found:\n• Status: %s", ord.Status)
		if ord.TrackingURL != "" {
			msg += "\n• Tracking: " + ord.TrackingURL
		}
		return msg, nil
	}

	// Zoho CRM (lead capture)
	if spec.ToolPlan.NeedsZohoCRM && rt.Tools != nil && rt.Tools.ZohoCRM != nil {
		crmID, err := rt.Tools.ZohoCRM.UpsertLeadOrContact(ctx, user, conv, facts)
		if err != nil {
			return "", err
		}
		_ = crmID // you can store this into app_users.crm_contact_id later via SB PATCH if you add that helper
		return "Thanks — I’ve saved your details. How would you like to proceed (product help, order status, or support)?", nil
	}

	// Zoho Desk (ticket)
	if spec.ToolPlan.NeedsZohoDesk && rt.Tools != nil && rt.Tools.ZohoDesk != nil {
		deskContactID, err := rt.Tools.ZohoDesk.EnsureContact(ctx, user, facts)
		if err != nil {
			return "", err
		}
		subject := "Support request"
		if intent == IntentReturnRefund {
			subject = "Return/Refund request"
		}
		desc := buildTicketDescription(conv.Summary, userText, facts)
		ticketID, err := rt.Tools.ZohoDesk.CreateTicket(ctx, deskContactID, subject, desc, map[string]string{
			"Conversation_ID": conv.ID,
			"Order_ID":        facts["order_id"],
			"Channel":         conv.Channel,
		})
		if err != nil {
			return "", err
		}
		if ticketID == "" {
			return "I’ve captured the details. A support agent will get back to you shortly.", nil
		}
		return "I’ve created a support ticket: " + ticketID + "\nWe’ll follow up soon.", nil
	}

	// Brevo email
	if spec.ToolPlan.NeedsBrevoEmail && rt.Tools != nil && rt.Tools.Brevo != nil {
		email := facts["email"]
		if email == "" {
			return "", fmt.Errorf("missing email for brevo")
		}
		_, err := rt.Tools.Brevo.SendTransactional(ctx, email, "Support update", "Thanks—we received your request.", map[string]string{
			"conversation_id": conv.ID,
		})
		if err != nil {
			return "", err
		}
		return "Done — I’ve sent the details to your email.", nil
	}

	// default: LLM response
	return rt.llmReply(intent, conv.Summary, recent, userText, facts), nil
}

func (rt *Router) llmReply(intent Intent, summary string, recent []MessageRow, userText string, facts map[string]string) string {
	system := "You are an ecommerce assistant. Be concise and helpful. " +
		"Never invent order status, delivery dates, refunds, or policies. " +
		"Keep every reply to 1-2 short sentences. " +
		"Ask at most one clarifying question at a time. " +
		"No preamble, no disclaimers, no repetition. " +
		"If info is missing, ask only the single most important missing field."

	h := make([]openAIChatMsg, 0, len(recent))
	for _, m := range recent {
		role := m.Role
		if role != "user" && role != "assistant" && role != "system" && role != "developer" {
			// safest fallback: assistant (prevents user content being treated as system)
			role = "assistant"
		}
		h = append(h, openAIChatMsg{Role: role, Content: m.Content})
	}

	// Add “facts” to system (short)
	factsLine := ""
	if v := facts["order_id"]; v != "" {
		factsLine += " order_id=" + v + ";"
	}
	if v := facts["email"]; v != "" {
		factsLine += " email=" + v + ";"
	}
	if v := facts["phone"]; v != "" {
		factsLine += " phone=" + v + ";"
	}
	if v := facts["name"]; v != "" {
		factsLine += " name=" + v + ";"
	}
	if factsLine != "" {
		system += " Known facts:" + factsLine
	}

	reply, err := rt.LLM.Chat(system, summary, h, userText)
	if err != nil {
		// Log full error server-side; keep user message friendly
		log.Println("OpenAI error:", err)
		return "Sorry — I ran into an error. Please try again."
	}
	return reply
}

func (rt *Router) persistAssistant(conv Conversation, intent Intent, reply string, facts map[string]string) error {
	_ = rt.SB.InsertMessage(conv.ID, "assistant", reply, map[string]any{"intent": intent})

	// rolling summary (simple + safe)
	newSummary := conv.Summary
	if newSummary != "" {
		newSummary += "\n"
	}
	newSummary += "A: " + reply
	if len(newSummary) > 1500 {
		newSummary = newSummary[len(newSummary)-1500:]
	}

	patch := map[string]any{
		"last_intent": string(intent),
		"summary":     newSummary,
		"metadata":    conv.Metadata,
	}
	return rt.SB.UpdateConversation(conv.ID, patch)
}

func buildTicketDescription(summary, lastUserText string, facts map[string]string) string {
	lines := []string{}
	if summary != "" {
		lines = append(lines, "Chat summary:\n"+summary)
	}
	lines = append(lines, "\nLatest message:\n"+lastUserText)

	if facts["order_id"] != "" {
		lines = append(lines, "\nOrder ID: "+facts["order_id"])
	}
	if facts["item"] != "" {
		lines = append(lines, "Item: "+facts["item"])
	}
	if facts["reason"] != "" {
		lines = append(lines, "Reason: "+facts["reason"])
	}
	return strings.Join(lines, "\n")
}

// -------- intent classification (simple heuristic) --------

func classifyIntent(text string) Intent {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "track") || strings.Contains(t, "where is my order") || strings.Contains(t, "delivery") || strings.Contains(t, "order status"):
		return IntentOrderStatus
	case strings.Contains(t, "return") || strings.Contains(t, "refund") || strings.Contains(t, "exchange") || strings.Contains(t, "cancel"):
		return IntentReturnRefund
	case strings.Contains(t, "complaint") || strings.Contains(t, "damaged") || strings.Contains(t, "wrong item") || strings.Contains(t, "not received"):
		return IntentComplaintSupport
	case strings.Contains(t, "bulk") || strings.Contains(t, "wholesale") || strings.Contains(t, "call me") || strings.Contains(t, "contact me"):
		return IntentLeadCapture
	case strings.Contains(t, "compare") || strings.Contains(t, "vs "):
		return IntentComparison
	case strings.Contains(t, "price") || strings.Contains(t, "in stock"):
		return IntentPricingAvailability
	case strings.Contains(t, "recommend") || strings.Contains(t, "suggest") || strings.Contains(t, "best for") || strings.Contains(t, "help me choose"):
		return IntentProductDiscovery
	default:
		return IntentOther
	}
}

// -------- facts extraction + requirements --------

var (
	reEmail = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	rePhone = regexp.MustCompile(`\b(\+?\d[\d\s\-]{8,}\d)\b`)
	reOrder = regexp.MustCompile(`(?i)\b(order\s*#?\s*|ord\s*#?\s*)([A-Z0-9\-]{4,})\b`)
)

func (rt *Router) extractFacts(ctx context.Context, in Inbound) (map[string]string, string) {
	f := map[string]string{}
	extractorErr := ""

	// WhatsApp provides a stable phone
	if in.WhatsAppFrom != "" {
		f["phone"] = normalizePhone(in.WhatsAppFrom)
	}

	// regex fallback extraction
	if m := reEmail.FindString(in.UserText); m != "" {
		f["email"] = normalizeEmail(m)
	}
	if m := rePhone.FindString(in.UserText); m != "" && f["phone"] == "" {
		f["phone"] = normalizePhone(m)
	}
	if m := reOrder.FindStringSubmatch(in.UserText); len(m) >= 3 {
		f["order_id"] = strings.TrimSpace(m[2])
	}

	lt := strings.ToLower(in.UserText)
	if strings.Contains(lt, "my name is") {
		idx := strings.Index(lt, "my name is")
		name := strings.TrimSpace(in.UserText[idx+len("my name is"):])
		if len(name) > 0 && len(name) < 60 {
			f["name"] = name
		}
	}

	// LLM extraction is forced to gpt-4.1-mini for stable schema extraction.
	if rt.LLM != nil {
		extracted, err := rt.LLM.ExtractFactsForStorage(ctx, in.UserText)
		if err != nil {
			log.Println("fact extraction error:", err)
			extractorErr = err.Error()
		} else {
			for k, v := range extracted {
				v = strings.TrimSpace(v)
				if v != "" {
					f[k] = v
				}
			}
		}
	}

	return f, extractorErr
}

func missingFields(spec IntentSpec, facts map[string]string, in Inbound) []Field {
	// RequiredAllOf
	var missing []Field
	for _, f := range spec.RequiredAllOf {
		if facts[string(f)] == "" {
			missing = append(missing, f)
		}
	}

	// RequiredAnyOf groups
	if len(spec.RequiredAnyOf) > 0 {
		satisfied := false
		for _, group := range spec.RequiredAnyOf {
			ok := true
			for _, f := range group {
				// WhatsApp: phone is implicitly present
				if f == FieldPhone && in.WhatsAppFrom != "" {
					continue
				}
				if facts[string(f)] == "" {
					ok = false
					break
				}
			}
			if ok {
				satisfied = true
				break
			}
		}
		if !satisfied {
			// choose first group fields as missing hint
			for _, f := range spec.RequiredAnyOf[0] {
				if facts[string(f)] == "" {
					missing = append(missing, f)
				}
			}
		}
	}

	return uniqueFields(missing)
}

func (rt *Router) askForMissing(spec IntentSpec, missing []Field) string {
	if len(spec.ClarifyQuestions) > 0 {
		// MVP: ask only the first clarifier to avoid overwhelming the user
		return spec.ClarifyQuestions[0]
	}
	// generic fallback
	names := []string{}
	for _, f := range missing {
		names = append(names, string(f))
	}
	return "I need a bit more info: " + strings.Join(names, ", ")
}

// --- metadata facts helpers ---

func getFactsFromMetadata(meta map[string]any) map[string]string {
	out := map[string]string{}
	if meta == nil {
		return out
	}
	raw, ok := meta["facts"]
	if !ok {
		return out
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func setFactsInMetadata(meta map[string]any, facts map[string]string) map[string]any {
	if meta == nil {
		meta = map[string]any{}
	}
	fm := map[string]any{}
	for k, v := range facts {
		if v != "" {
			fm[k] = v
		}
	}
	meta["facts"] = fm
	return meta
}
