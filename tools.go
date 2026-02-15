package main

import "context"

type ShopifyOrder struct {
	OrderID     string
	Status      string
	TrackingURL string
}

type Tools struct {
	Shopify  ShopifyClient
	ZohoCRM  ZohoCRMClient
	ZohoDesk ZohoDeskClient
	Brevo    BrevoClient
	WhatsApp WhatsAppClient
}

type ShopifyClient interface {
	LookupOrder(ctx context.Context, identifiers map[string]string) (*ShopifyOrder, error)
}

type ZohoCRMClient interface {
	UpsertLeadOrContact(ctx context.Context, user AppUser, conv Conversation, facts map[string]string) (crmID string, err error)
	AddNote(ctx context.Context, crmID string, note string) error
}

type ZohoDeskClient interface {
	EnsureContact(ctx context.Context, user AppUser, facts map[string]string) (deskContactID string, err error)
	CreateTicket(ctx context.Context, deskContactID string, subject string, description string, customFields map[string]string) (ticketID string, err error)
}

type BrevoClient interface {
	SendTransactional(ctx context.Context, toEmail string, subject string, text string, meta map[string]string) (messageID string, err error)
}

type WhatsAppClient interface {
	SendText(ctx context.Context, toPhone string, text string) error
}

// ---- Minimal no-op stubs so the project runs without integrations ----

type NoopShopify struct{}

func (n NoopShopify) LookupOrder(ctx context.Context, identifiers map[string]string) (*ShopifyOrder, error) {
	return nil, nil // implement later
}

type NoopZohoCRM struct{}

func (n NoopZohoCRM) UpsertLeadOrContact(ctx context.Context, user AppUser, conv Conversation, facts map[string]string) (string, error) {
	return "", nil
}
func (n NoopZohoCRM) AddNote(ctx context.Context, crmID string, note string) error { return nil }

type NoopZohoDesk struct{}

func (n NoopZohoDesk) EnsureContact(ctx context.Context, user AppUser, facts map[string]string) (string, error) {
	return "", nil
}
func (n NoopZohoDesk) CreateTicket(ctx context.Context, deskContactID string, subject string, description string, customFields map[string]string) (string, error) {
	return "", nil
}

type NoopBrevo struct{}

func (n NoopBrevo) SendTransactional(ctx context.Context, toEmail string, subject string, text string, meta map[string]string) (string, error) {
	return "", nil
}

type NoopWhatsApp struct{}

func (n NoopWhatsApp) SendText(ctx context.Context, toPhone string, text string) error { return nil }
