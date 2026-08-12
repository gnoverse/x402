package facilitator

import (
	"encoding/base64"

	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
)

// Scheme-level invalid-reason codes, reported when the gno exact scheme's own
// rules refuse a payment.
//
// The naming follows the peer schemes: invalid_exact_<caip2-namespace>, with
// payload_ for what the payload itself says and no payload_ for what the chain
// says about the transaction. "gno" is the CAIP-2 namespace — SVM's constants
// likewise say "solana", not "svm".
const (
	ReasonMalformedTransaction = "invalid_exact_gno_payload_transaction_could_not_be_decoded"
	ReasonUnexpectedMessage    = "invalid_exact_gno_payload_unexpected_message"
	ReasonSignatureCount       = "invalid_exact_gno_payload_signature_count"
	ReasonRecipientMismatch    = "invalid_exact_gno_payload_recipient_mismatch"
	ReasonAmountMismatch       = "invalid_exact_gno_payload_amount_mismatch"
	ReasonMemoMismatch         = "invalid_exact_gno_payload_memo_mismatch"
	ReasonChainMismatch        = "invalid_exact_gno_network_mismatch"
	ReasonSimulationFailed     = "invalid_exact_gno_transaction_simulation_failed"
	ReasonBroadcastFailed      = "invalid_exact_gno_transaction_failed"

	// ReasonSignatureInvalid and ReasonSequenceMismatch report what the signer's
	// on-chain account decides, which is checked before a payment is simulated or
	// settled: gno's auth ante skips signature verification under simulate, so
	// simulation alone passes a forged signature and a superseded one alike.
	//
	// A signature the account's key and sequence do not satisfy is invalid. One
	// that satisfies the sequence the account has already consumed is a replay of
	// a spent payment, and it reports separately because the payer's remedy
	// differs: re-sign for this chain and account, versus sign a fresh payment.
	// Neither may be folded into the other — a published reason vocabulary cannot
	// change without breaking the integrators that read it.
	ReasonSignatureInvalid = "invalid_exact_gno_payload_signature_invalid"
	ReasonSequenceMismatch = "invalid_exact_gno_payload_sequence_mismatch"
)

// Envelope-level invalid-reason codes. These are the spec's own §9 names and
// carry no scheme prefix: they describe the transport and the envelope, which
// every scheme shares.
const (
	// ReasonInvalidPayload reports a payload the server cannot act on — a
	// PAYMENT-SIGNATURE header it cannot parse, or an accepted object that
	// contradicts the requirements it answers.
	ReasonInvalidPayload = "invalid_payload"

	// ReasonInvalidVersion reports a payload declaring a protocol version this
	// implementation does not speak.
	ReasonInvalidVersion = "invalid_x402_version"

	// ReasonInvalidRequirements reports a requirements object this scheme
	// cannot act on — an extra.memo that is not a string, or one over the cap.
	ReasonInvalidRequirements = "invalid_payment_requirements"

	// ReasonUnsupportedScheme reports requirements naming a scheme other than
	// "exact".
	ReasonUnsupportedScheme = "unsupported_scheme"

	// ReasonUnexpectedSettleError reports that settlement could not be reached
	// or did not answer usefully, so the payment's outcome is unknown.
	//
	// This seller never emits it, and the constant is kept for a client reading
	// the vocabulary of a peer that does: an unknown outcome is not a verdict
	// here, so it is answered 503 with no reason code rather than a 402 carrying
	// one. A reason code names something established about the payment.
	ReasonUnexpectedSettleError = "unexpected_settle_error"
)

// VerifyStatic checks every spec rule decidable without chain state and
// returns the decoded tx and the payer (bech32 master address funds move
// from) on success, or a non-empty invalidReason code on failure.
//
// Session scope, spend limit and balance are checked by simulating against a
// node. Neither this function nor simulation decides the signature or the
// sequence: nothing here reads chain state, and gno's auth ante skips signature
// verification under simulate. Both are verified against the signer's on-chain
// account — verifyAccountChecks, run on the transaction this function returns and
// ahead of any simulation — and CheckTx verifies them once more at settle.
func VerifyStatic(req PaymentRequirements, payload SchemePayload) (tx *std.Tx, payer, reason string) {
	raw, err := base64.StdEncoding.DecodeString(payload.Transaction)
	if err != nil {
		return nil, "", ReasonMalformedTransaction
	}
	var decoded std.Tx
	if err := amino.Unmarshal(raw, &decoded); err != nil {
		return nil, "", ReasonMalformedTransaction
	}
	if len(decoded.Signatures) != 1 {
		return nil, "", ReasonSignatureCount
	}
	// One signature may still carry a threshold key holding thousands of
	// subkeys, each costing a verification. The chain counts subkeys in its
	// first ante check, before it reads any account, and this check keeps that
	// ordering: static verification runs ahead of simulation precisely so a
	// payment is refused before it costs anything. std.CountSubKeys reports 1
	// for an absent key, which is the shape a signature takes when the account
	// already stores the key it was signed with.
	if std.CountSubKeys(decoded.Signatures[0].PubKey) != 1 {
		return nil, "", ReasonSignatureCount
	}
	if len(decoded.Msgs) != 1 {
		return nil, "", ReasonUnexpectedMessage
	}
	send, ok := decoded.Msgs[0].(bank.MsgSend)
	if !ok {
		return nil, "", ReasonUnexpectedMessage
	}
	if send.ToAddress.String() != req.PayTo {
		return nil, "", ReasonRecipientMismatch
	}
	want, err := std.ParseCoins(req.Amount + req.Asset)
	if err != nil || len(want) != 1 || len(send.Amount) != 1 || send.Amount[0] != want[0] {
		return nil, "", ReasonAmountMismatch
	}
	memo, err := req.Memo()
	if err != nil {
		return nil, "", ReasonInvalidRequirements
	}
	if memo != "" && decoded.Memo != memo {
		return nil, "", ReasonMemoMismatch
	}
	return &decoded, send.FromAddress.String(), ""
}
