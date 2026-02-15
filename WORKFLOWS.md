# AI Chatbot Workflows (MVP → Full)

This document defines workflows and acceptance criteria for the Go chatbot.
We implement workflows in the order listed (dependency-safe).

## Shared Workflow Pattern (applies to every workflow)

Every workflow must follow this pattern:

1) **Trigger**
   - What user says or what event comes in (web chat / WhatsApp webhook / etc.)

2) **Identity resolution**
   - Determine identity class: new visitor, returning anonymous, returning identified, authenticated, known customer with orders
   - Handle identity conflicts (shared device)

3) **Data operations (Supabase)**
   - Read/write tables as needed
   - Keep Supabase as source-of-truth for session + chat + identity mapping

4) **Decision**
   - Intent classification
   - Validate required fields
   - Prompt for missing fields using concise questions

5) **Tools (Integrations)**
   - Shopify / Zoho CRM / Zoho Desk / Brevo / WhatsApp
   - Must log tool requests/responses to `tool_calls`

6) **LLM response**
   - Used only for natural-language responses and guardrails
   - Never invent order status/dates/refunds/policies
   - If data missing, ask for it; if tool fails, fallback safely

7) **Persist**
   - Always store inbound/outbound `messages`
   - Update `conversations.summary` and `conversations.metadata`
   - Write `events` for key lifecycle actions
   - Ensure idempotency for WhatsApp retries via `idempotency_keys`

---

## Sources of Truth

- **Supabase**: sessions, identity mapping, chat history, conversation summary, events, tool_calls
- **Shopify**: customers, orders, fulfillment, refunds/returns
- **Zoho CRM**: leads/contacts, pipeline, consent, enrichment fields
- **Zoho Desk**: tickets, SLA, support history
- **Brevo**: transactional emails

**Rule:** Do not store full order/ticket details in Supabase. Store references (IDs) and last lookup timestamps.

---

## Identity Model (Shared across all workflows)

### Identity classes

- **Tier 0: Anonymous**
  - Identified only by `session_id` (cookie sid) or `wa:<phone>`
- **Tier 1: Named**
  - Has a name only (not unique)
- **Tier 2: Contact Identified**
  - Has email and/or phone (not yet verified)
- **Tier 3: Verified Contact**
  - Email/phone verified via order lookup or OTP (OTP later)
- **Tier 4: Authenticated**
  - Known platform customer ID (e.g., Shopify customer id)
- **Tier 5: Known Customer Ordered**
  - Authenticated + orders_count > 0

### Identity keys (in `identity_keys`)
Supported key types:
- `cookie_sid`
- `email` (normalized lowercase)
- `phone` (normalized E.164 or consistent digits-only)
- `shopify_customer_id`
- `order_id`
- `zoho_contact_id`
- `zoho_desk_contact_id`

### Session mapping (in `user_sessions`)
- `session_id` is the primary session key.
- A session maps to exactly one `app_users.id` at a time.

### Conflict policy (MVP default)
If an identifier (email/phone/customer_id) maps to a *different* user than the current session user:
- Do **not** auto-switch silently.
- Ask user to confirm:
  - “I found a different account for that email/phone. Do you want to switch to it?”
  - Options: Switch / Continue as guest
- Log an `events` row: `identity.conflict`

---

## Workflow Order (Do in this order)

WF-01 Identity & Session  
WF-02 General Product/Help Chat (LLM-only, stores chat)  
WF-03 Lead Capture → Zoho CRM  
WF-04 Order Status → Shopify (verify order_id OR email/phone)  
WF-05 Returns/Refund → Zoho Desk ticket (+ optional Shopify lookup)  
WF-06 Proactive Resume (returning user: summarize + continue)

---

# WF-01 Identity & Session

## Trigger
Any inbound message:
- Web: `POST /chat`
- WhatsApp: webhook → normalized inbound → same internal handler

## Identity resolution rules
Input:
- `session_id` (cookie sid or `wa:<phone>`)
- `channel`
- `userText` (for extracting email/phone/name/order id)
- optional: `shopify_customer_id` when logged-in integration exists

Algorithm:
1) Look up `user_sessions.session_id`
   - If found: use `user_id` (returning)
   - Else: create new `app_users` (anonymous) + insert `user_sessions`

2) Extract identity candidates from `userText`:
   - email, phone, name, order_id (optional)

3) For each candidate key (strongest to weakest):
   - Check `identity_keys(key_type,key_value)` for an existing `user_id`
   - If found and differs from current session user → **conflict flow**
   - If not found → insert identity key linked to current user

4) Update `app_users`:
   - set `email/phone/name` if newly found
   - compute `identity_tier`, `identity_status`, `confidence_score`, `primary_identifier`
   - update `last_seen_at`

5) Update `user_sessions.last_seen_at`

## Data operations (Supabase)
Reads:
- `user_sessions` by session_id
- `identity_keys` by (key_type, key_value)

Writes:
- insert `app_users` (new)
- upsert/insert `user_sessions`
- insert `identity_keys`
- update `app_users`
- insert `events`:
  - `identity.resolved`
  - `identity.key_added`
  - `identity.conflict`

## Done when (acceptance tests)
- New visitor creates:
  - `app_users(identity_tier=0)`
  - `user_sessions(session_id -> user_id)`
- Returning visitor with same cookie:
  - resolves same `user_id`
- Returning visitor provides email:
  - adds identity_key(email) and updates app_users.email and tier
- Email belongs to different user:
  - returns conflict prompt and logs `identity.conflict`

---

# WF-02 General Product/Help Chat (LLM-only)

## Trigger
User asks about products, usage, ingredients, general help, comparisons, pricing (if not requiring real-time inventory).

## Identity resolution
Always run WF-01 first.

## Data operations (Supabase)
- Get or create an open `conversations` row for the user + channel
- Insert inbound `messages`
- Fetch recent message history
- Update `conversations.summary` and `metadata` after assistant reply
- Insert `events`: `chat.message_received`, `chat.reply_sent`

## Decision
- classify intent as one of:
  - ProductDiscovery, Comparison, PricingAvailability, Other
- if missing basic context (goal/age/skin type etc.), ask 1 question max per turn

## Tools
None in MVP. (Later: product catalog, inventory, pricing.)

## LLM response
- Guardrails: no medical claims, no invented prices/inventory, no policy promises.

## Persist
- Always store messages and summary.

## Done when
- User sees a helpful answer
- Supabase stores messages + updated summary

---

# WF-03 Lead Capture → Zoho CRM

## Trigger
User asks: “call me”, “bulk”, “wholesale”, “contact me”, “quote”, “demo”.

## Identity resolution
WF-01 first.
If anonymous: capture minimum contact details.

## Data operations (Supabase)
- Store `name`, `email` or `phone` in `app_users`
- Add `identity_keys` for email/phone
- Log `events`: `lead.capture_started`, `lead.capture_completed`

## Decision (required fields)
Need:
- name (optional but preferred)
- email OR phone (at least one)
- interest (bulk/wholesale/product inquiry) from message or 1 question

## Tools
- Zoho CRM:
  - upsert lead/contact
  - store zoho id back to `app_users.crm_contact_id`
- Log tool_calls

## LLM response
- Keep short. Confirm what was saved and what will happen next.

## Done when
- Zoho CRM contact/lead created
- app_users updated with crm_contact_id
- user gets confirmation message

---

# WF-04 Order Status → Shopify

## Trigger
User asks: track order, delivery status, order status, shipment, not received.

## Identity resolution
WF-01 first.

## Decision (required fields)
Need one of:
- order_id, OR
- (email or phone) used at checkout

## Tools
- Shopify lookup:
  - find order by order_id
  - or search orders by email/phone
- Log tool_calls

## LLM response
- Only present data returned by Shopify.
- If not found, ask for alternate identifier.

## Persist
- Store reference fields in conversation metadata: last_order_id, last_lookup_at
- Insert events: `order.lookup_requested`, `order.lookup_succeeded`, `order.lookup_failed`

## Done when
- Correct status + tracking shown (if available) OR clear next ask

---

# WF-05 Returns/Refund → Zoho Desk (+ optional Shopify)

## Trigger
return/refund/exchange/cancel, damaged, wrong item, complaint.

## Identity resolution
WF-01 first.

## Decision (required fields)
Prefer:
- order_id (or email/phone)
- item
- reason
- preferred resolution: return/refund/exchange (optional)

## Tools
- Optional Shopify order lookup (to validate order)
- Zoho Desk:
  - ensure contact
  - create ticket with conversation summary + latest message + identifiers
- Optional Brevo: send confirmation email
- Log tool_calls

## LLM response
- Confirm ticket created, what happens next, what info is needed (if any).

## Persist
- Save ticket_id in conversation metadata (and optionally app_users)
- Events: `ticket.created`, `ticket.failed`

## Done when
- Ticket created and user receives confirmation

---

# WF-06 Proactive Resume (returning user)

## Trigger
Returning session opens chat after > X minutes/hours/days, or user says “continue”.

## Identity resolution
WF-01 first.

## Data ops
- Fetch last open conversation
- Use conversation summary + last intent

## Decision
- Provide 1–2 line recap and a next-step question.
- If last workflow needed missing fields, re-ask the next missing field.

## Tools
None unless user asks.

## LLM response
- Keep brief and action-oriented.

## Done when
- Returning users immediately see context + next step prompt
