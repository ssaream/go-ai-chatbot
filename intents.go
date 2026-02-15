package main

type Field string

const (
	FieldOrderID Field = "order_id"
	FieldEmail   Field = "email"
	FieldPhone   Field = "phone"
	FieldName    Field = "name"
	FieldItem    Field = "item"
	FieldReason  Field = "reason"
)

type Intent string

const (
	IntentProductDiscovery    Intent = "product_discovery"
	IntentProductQuestion     Intent = "product_question"
	IntentComparison          Intent = "comparison"
	IntentPricingAvailability Intent = "pricing_availability"
	IntentOrderStatus         Intent = "order_status"
	IntentCancelOrder         Intent = "cancel_order"
	IntentChangeOrder         Intent = "change_order"
	IntentReturnRefund        Intent = "return_refund"
	IntentRefundStatus        Intent = "refund_status"
	IntentShippingInfo        Intent = "shipping_info"
	IntentComplaintSupport    Intent = "complaint_support"
	IntentLeadCapture         Intent = "lead_capture"
	IntentHandoffHuman        Intent = "handoff_human"
	IntentOther               Intent = "other"
)

type IntentSpec struct {
	Intent           Intent
	RequiredAnyOf    [][]Field // any one group satisfies: [[order_id],[email,phone]] etc.
	RequiredAllOf    []Field   // must all be present
	MaxClarifyQs     int
	ClarifyQuestions []string
	ToolPlan         ToolPlan
}

type ToolPlan struct {
	NeedsShopifyLookup bool
	NeedsZohoCRM       bool
	NeedsZohoDesk      bool
	NeedsBrevoEmail    bool
}

func RoutingTable() map[Intent]IntentSpec {
	return map[Intent]IntentSpec{
		IntentProductDiscovery: {
			Intent:       IntentProductDiscovery,
			MaxClarifyQs: 2,
			ClarifyQuestions: []string{
				"What’s your goal (e.g., bone health, sleep, immunity)?",
				"Any preferences (budget, form, allergies, vegetarian/vegan)?",
			},
		},
		IntentOrderStatus: {
			Intent: IntentOrderStatus,
			RequiredAnyOf: [][]Field{
				{FieldOrderID},
				{FieldEmail},
				{FieldPhone},
			},
			MaxClarifyQs: 2,
			ClarifyQuestions: []string{
				"Please share your Order ID (best). If you don’t have it, share the email or phone used at checkout.",
				"If multiple orders exist, please share the approximate order date.",
			},
			ToolPlan: ToolPlan{NeedsShopifyLookup: true},
		},
		IntentReturnRefund: {
			Intent: IntentReturnRefund,
			RequiredAllOf: []Field{
				FieldOrderID, FieldItem, FieldReason,
			},
			MaxClarifyQs: 2,
			ClarifyQuestions: []string{
				"Please share the Order ID.",
				"Which item is it, and what’s the reason for return/refund/exchange?",
			},
			ToolPlan: ToolPlan{NeedsZohoDesk: true},
		},
		IntentComplaintSupport: {
			Intent:        IntentComplaintSupport,
			RequiredAnyOf: [][]Field{{FieldPhone}, {FieldEmail}}, // WhatsApp will already have phone
			MaxClarifyQs:  2,
			ClarifyQuestions: []string{
				"Sorry about that—can you share your Order ID (if applicable) and what went wrong?",
				"What’s the best contact method—email or phone?",
			},
			ToolPlan: ToolPlan{NeedsZohoDesk: true},
		},
		IntentLeadCapture: {
			Intent:        IntentLeadCapture,
			RequiredAnyOf: [][]Field{{FieldPhone}, {FieldEmail}},
			MaxClarifyQs:  2,
			ClarifyQuestions: []string{
				"Sure—what’s the best email or phone number to reach you?",
				"May I have your name as well?",
			},
			ToolPlan: ToolPlan{NeedsZohoCRM: true},
		},
		IntentOther: {
			Intent:       IntentOther,
			MaxClarifyQs: 1,
			ClarifyQuestions: []string{
				"Is this about (1) choosing a product, (2) order status, or (3) returns/support?",
			},
		},
	}
}
