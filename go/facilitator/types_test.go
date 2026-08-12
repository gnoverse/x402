package facilitator

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPaymentRequirements_JSONFieldNames(t *testing.T) {
	req := PaymentRequirements{
		Scheme:            "exact",
		Network:           "gno:topaz-1",
		Amount:            "250000",
		Asset:             "ugnot",
		PayTo:             "g1payto",
		MaxTimeoutSeconds: 60,
		Extra:             map[string]any{"memo": "pay_1"},
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)
	for _, field := range []string{`"scheme"`, `"network"`, `"amount"`, `"asset"`, `"payTo"`, `"maxTimeoutSeconds"`, `"memo"`} {
		assert.Contains(t, string(data), field)
	}
	for _, gone := range []string{`"gasWanted"`, `"gasFee"`} {
		assert.NotContains(t, string(data), gone, "the scheme declares no gas fields")
	}
}

// TestPaymentRequirements_MaxTimeoutSecondsAlwaysEmitted pins the field's
// presence: a client computing a deadline needs to distinguish an advertised
// value from an absent one, so the key may never be elided.
func TestPaymentRequirements_MaxTimeoutSecondsAlwaysEmitted(t *testing.T) {
	data, err := json.Marshal(PaymentRequirements{})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"maxTimeoutSeconds"`)
}

// TestPaymentRequirements_ExtraKeepsUnknownKeys proves the echo is not lossy:
// a client receives requirements whose extra carries scheme keys it does not
// know, and must echo them back unchanged in the payload's accepted object.
func TestPaymentRequirements_ExtraKeepsUnknownKeys(t *testing.T) {
	const raw = `{"scheme":"exact","network":"gno:dev","amount":"1","asset":"ugnot","payTo":"g1x",` +
		`"maxTimeoutSeconds":60,"extra":{"memo":"pay_1","name":"USDC","version":"2"}}`

	var req PaymentRequirements
	require.NoError(t, json.Unmarshal([]byte(raw), &req))

	data, err := json.Marshal(req)
	require.NoError(t, err)
	for _, field := range []string{`"memo":"pay_1"`, `"name":"USDC"`, `"version":"2"`} {
		assert.Contains(t, string(data), field, "an unknown extra key must survive the round trip")
	}
}

func TestPaymentRequirements_Memo(t *testing.T) {
	atCap := strings.Repeat("a", maxMemoBytes)

	cases := []struct {
		name    string
		extra   map[string]any
		want    string
		wantErr bool
	}{
		{name: "no extra"},
		{name: "empty extra", extra: map[string]any{}},
		{name: "no memo key", extra: map[string]any{"name": "USDC"}},
		{name: "memo present", extra: map[string]any{"memo": "pay_1"}, want: "pay_1"},
		{name: "memo at the cap", extra: map[string]any{"memo": atCap}, want: atCap},
		{name: "memo over the cap", extra: map[string]any{"memo": atCap + "a"}, wantErr: true},
		// A non-string memo must be a hard error, never a silent "": treating it
		// as absent would drop the memo requirement entirely and hand back a
		// payment the seller then rejects.
		{name: "memo is a number", extra: map[string]any{"memo": 123}, wantErr: true},
		{name: "memo is null", extra: map[string]any{"memo": nil}, wantErr: true},
		{name: "memo is an object", extra: map[string]any{"memo": map[string]any{"a": "b"}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := reqFixture()
			req.Extra = tc.extra
			got, err := req.Memo()
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, got, "a rejected memo must not also be returned")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSettleResponse_JSONFieldNames(t *testing.T) {
	resp := SettleResponse{
		Success:     true,
		Transaction: "abc123",
		Network:     "gno:dev",
		Payer:       "g1payer",
		ErrorReason: ReasonBroadcastFailed,
	}
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	for _, field := range []string{`"success"`, `"transaction"`, `"network"`, `"payer"`, `"errorReason"`} {
		assert.Contains(t, string(data), field)
	}
}

// TestSettleResponse_FailureCarriesEmptyTransaction pins the spec's "empty
// string if settlement failed": the key is always present, so a client never
// has to distinguish absent from empty.
func TestSettleResponse_FailureCarriesEmptyTransaction(t *testing.T) {
	data, err := json.Marshal(SettleResponse{Success: false, Network: "gno:dev", ErrorReason: ReasonBroadcastFailed})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"transaction":""`)
}

func TestVerifyResponse_JSONFieldNames(t *testing.T) {
	resp := VerifyResponse{
		IsValid:       false,
		InvalidReason: ReasonChainMismatch,
		Payer:         "g1payer",
	}
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	for _, field := range []string{`"isValid"`, `"invalidReason"`, `"payer"`} {
		assert.Contains(t, string(data), field)
	}
}

// TestSupportedResponse_EmptySetsAreEmptyNotNull pins that a keyless
// facilitator states "no extensions, no signers" as a property. Marshalling
// nil slices/maps would emit null, which reads as "not answered".
func TestSupportedResponse_EmptySetsAreEmptyNotNull(t *testing.T) {
	resp := SupportedResponse{
		Kinds:      []SupportedKind{{X402Version: 2, Scheme: "exact", Network: "gno:dev"}},
		Extensions: []string{},
		Signers:    map[string][]string{},
	}
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	for _, field := range []string{`"kinds"`, `"x402Version"`, `"scheme"`, `"network"`, `"extensions":[]`, `"signers":{}`} {
		assert.Contains(t, string(data), field)
	}
}

// TestPaymentPayload_RoundTrip pins that a payload reaching the facilitator over
// the settle wire comes back out equal to what a buyer sent, extras and all: the
// requirements it carries are what verification is decided against.
func TestPaymentPayload_RoundTrip(t *testing.T) {
	payload := PaymentPayload{
		X402Version: 2,
		Resource:    &ResourceInfo{URL: "/premium", MimeType: "application/json"},
		Accepted: PaymentRequirements{
			Scheme: "exact", Network: "gno:dev", Amount: "1", Asset: "ugnot", PayTo: "g1x",
			MaxTimeoutSeconds: 60,
			Extra:             map[string]any{"memo": "pay_1", "name": "USDC"},
		},
		Payload: SchemePayload{Transaction: "dGVzdA=="},
	}
	data, err := json.Marshal(payload)
	require.NoError(t, err)

	var got PaymentPayload
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, payload, got)
}

// TestSchemePayload_ForeignSchemeSurvivesTheHop is the rule that lets one seller take
// payment on more than one chain: a payload belonging to a scheme this implementation
// does not implement passes through untouched.
//
// A seller decodes the payload to find which offer was accepted, then re-encodes it for
// the facilitator. Reading an EVM EIP-3009 authorization through the gno scheme's single
// transaction field yields nothing, and re-encoding it produces {"transaction":""} — a
// settle request with the payment deleted from it. The facilitator then answers about a
// payload the buyer never sent.
func TestSchemePayload_ForeignSchemeSurvivesTheHop(t *testing.T) {
	const evmPayload = `{"signature":"0x49af5dc6","authorization":{"from":"0xBuyer",` +
		`"to":"0xSeller","value":"600000","validAfter":"0","validBefore":"1785961703",` +
		`"nonce":"0x1c72c955"}}`

	t.Run("a foreign payload re-emits byte for byte", func(t *testing.T) {
		var payload SchemePayload
		require.NoError(t, json.Unmarshal([]byte(evmPayload), &payload))
		assert.Empty(t, payload.Transaction, "an EVM payload carries no gno transaction")

		out, err := json.Marshal(payload)
		require.NoError(t, err)
		assert.JSONEq(t, evmPayload, string(out),
			"every field of the payment must reach the facilitator")
	})

	t.Run("a whole payment payload keeps its foreign scheme payload", func(t *testing.T) {
		// The shape a client actually sends: the envelope decoded, then re-marshalled
		// on the way to the facilitator.
		raw := `{"x402Version":2,"accepted":{"scheme":"exact","network":"eip155:84532",` +
			`"amount":"600000","asset":"0xUSDC","payTo":"0xSeller","maxTimeoutSeconds":60},` +
			`"payload":` + evmPayload + `}`
		var decoded PaymentPayload
		require.NoError(t, json.Unmarshal([]byte(raw), &decoded))

		out, err := json.Marshal(decoded)
		require.NoError(t, err)
		var got struct {
			Payload json.RawMessage `json:"payload"`
		}
		require.NoError(t, json.Unmarshal(out, &got))
		assert.JSONEq(t, evmPayload, string(got.Payload))
	})

	t.Run("a gno payload stays comparable with one built in process", func(t *testing.T) {
		var payload SchemePayload
		require.NoError(t, json.Unmarshal([]byte(`{"transaction":"dGVzdA=="}`), &payload))
		assert.Equal(t, SchemePayload{Transaction: "dGVzdA=="}, payload,
			"the payload this scheme does understand must not carry hidden state")
	})

	t.Run("a transaction of the wrong type is an error, not an empty one", func(t *testing.T) {
		var payload SchemePayload
		err := json.Unmarshal([]byte(`{"transaction":42}`), &payload)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "want a string")
	})
}

// TestFacilitatorRequest_CarriesX402Version pins the top-level version on the
// facilitator wire, alongside the payload's own.
func TestFacilitatorRequest_CarriesX402Version(t *testing.T) {
	data, err := json.Marshal(Request{
		X402Version:         2,
		PaymentPayload:      PaymentPayload{X402Version: 2, Accepted: reqFixture()},
		PaymentRequirements: reqFixture(),
	})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"x402Version":2`)
	assert.Contains(t, string(data), `"paymentPayload"`)
	assert.Contains(t, string(data), `"paymentRequirements"`)
}
