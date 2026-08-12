package facilitator

import (
	"encoding/json"
	"fmt"
)

const (
	// protocolVersion is the x402 version this implementation speaks; it is
	// the only version it accepts.
	protocolVersion = 2

	// schemeExact is the only payment scheme this implementation supports.
	schemeExact = "exact"

	// maxMemoBytes caps extra.memo; a longer memo would bloat the signed tx
	// and every echo of the requirements.
	maxMemoBytes = 256
)

// PaymentRequirements is one entry of a 402 response's accepts array. Extra is
// an open map so that scheme keys this implementation does not know survive the
// round trip into the payload's accepted object.
type PaymentRequirements struct {
	Scheme            string         `json:"scheme"`
	Network           string         `json:"network"`
	Amount            string         `json:"amount"`
	Asset             string         `json:"asset"`
	PayTo             string         `json:"payTo"`
	MaxTimeoutSeconds int            `json:"maxTimeoutSeconds"`
	Extra             map[string]any `json:"extra,omitempty"`
}

// Memo returns extra.memo, the memo the requirements bind the payment's
// transaction to, and "" when they bind none. A memo that is present but not a
// string, or longer than maxMemoBytes, is an error rather than an absent memo:
// reading either as "" would silently drop the binding and yield a payment the
// seller rejects. This is the single enforcement point for the cap.
func (r PaymentRequirements) Memo() (string, error) {
	v, ok := r.Extra["memo"]
	if !ok {
		return "", nil
	}
	memo, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("x402: extra.memo is %T, want string", v)
	}
	if len(memo) > maxMemoBytes {
		return "", fmt.Errorf("x402: extra.memo is %d bytes, exceeding the %d-byte maximum", len(memo), maxMemoBytes)
	}
	return memo, nil
}

// ResourceInfo describes the resource a payment buys, as the payload's envelope
// echoes it back.
type ResourceInfo struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// SchemePayload is a scheme's own payload object, carried verbatim.
//
// The gno exact scheme puts the base64 of an amino-encoded, fully signed std.Tx in
// transaction, and Transaction is that field. Other chains' exact schemes carry other
// things: EVM's EIP-3009 flow sends an authorization object and a signature, and no
// transaction at all.
//
// A seller advertising several networks forwards a payload it cannot itself read, so
// the bytes are kept as received and re-emitted unchanged. Re-encoding through this
// scheme's own shape would hand the facilitator {"transaction":""} — a settle request
// with the payment removed from it, which the facilitator can only refuse or, worse,
// answer about something else.
type SchemePayload struct {
	Transaction string

	// raw is the payload exactly as it arrived, or empty when this value was built in
	// process rather than decoded.
	raw json.RawMessage
}

// MarshalJSON re-emits a decoded payload byte for byte, and encodes a locally built
// one as this scheme's single transaction field.
func (p SchemePayload) MarshalJSON() ([]byte, error) {
	if len(p.raw) > 0 {
		return p.raw, nil
	}
	return json.Marshal(struct {
		Transaction string `json:"transaction"`
	}{Transaction: p.Transaction})
}

// UnmarshalJSON reads this scheme's transaction field, if the payload has one, and
// keeps the bytes when re-encoding them through that one field would lose something.
func (p *SchemePayload) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	tx, isGno := fields["transaction"]
	if isGno {
		if err := json.Unmarshal(tx, &p.Transaction); err != nil {
			return fmt.Errorf("x402: payload transaction is %s, want a string: %w", tx, err)
		}
	}
	// A payload that is exactly this scheme's own re-encodes identically, so keeping
	// its bytes would only make a decoded value compare unequal to the same payload
	// built in process. Anything else is kept: the fields below are the payment.
	// Cloned, because the decoder is free to reuse the buffer behind data.
	if !isGno || len(fields) != 1 {
		p.raw = append(json.RawMessage(nil), data...)
	}
	return nil
}

// PaymentPayload is the envelope a buyer sends in the PAYMENT-SIGNATURE header
// and a seller relays to this facilitator.
//
// The asymmetry between Resource and the absent extensions field is
// intentional. Resource is declared because the spec's own PAYMENT-SIGNATURE
// fixture carries one, so the field is what lets a conformant peer's payload
// decode without silently losing it. Nothing makes that demand of extensions,
// and a field no code fills, reads or honors misleads integrators — the reason
// extra.gasWanted and extra.gasFee are absent too.
type PaymentPayload struct {
	X402Version int                 `json:"x402Version"`
	Resource    *ResourceInfo       `json:"resource,omitempty"`
	Accepted    PaymentRequirements `json:"accepted"`
	Payload     SchemePayload       `json:"payload"`
}

// SettleResponse reports a settlement outcome. Transaction is always present —
// the empty string when settlement failed — so a client never has to tell an
// absent hash from an empty one.
type SettleResponse struct {
	Success     bool   `json:"success"`
	Transaction string `json:"transaction"` // hex tx hash
	Network     string `json:"network"`
	Payer       string `json:"payer,omitempty"`
	ErrorReason string `json:"errorReason,omitempty"`
}

// SupportedKind is one scheme/network pair a facilitator serves.
type SupportedKind struct {
	X402Version int    `json:"x402Version"`
	Scheme      string `json:"scheme"`
	Network     string `json:"network"`
}

// SupportedResponse answers GET /supported. Extensions and Signers are
// required and legitimately empty here: this facilitator implements no protocol
// extension, and it is keyless, so it holds no signer address to publish.
type SupportedResponse struct {
	Kinds      []SupportedKind     `json:"kinds"`
	Extensions []string            `json:"extensions"`
	Signers    map[string][]string `json:"signers"`
}

// VerifyResponse reports a verification outcome.
type VerifyResponse struct {
	IsValid       bool   `json:"isValid"`
	InvalidReason string `json:"invalidReason,omitempty"`
	Payer         string `json:"payer,omitempty"`
}
