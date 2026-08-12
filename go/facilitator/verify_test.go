package facilitator

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/gnolang/gno/gno.land/pkg/sdk/vm"
	"github.com/gnolang/gno/tm2/pkg/amino"
	"github.com/gnolang/gno/tm2/pkg/crypto"
	"github.com/gnolang/gno/tm2/pkg/crypto/ed25519"
	"github.com/gnolang/gno/tm2/pkg/crypto/multisig"
	"github.com/gnolang/gno/tm2/pkg/sdk/bank"
	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txFixture builds a signed-shape send tx and returns its base64 amino encoding.
// Signature content is not verified statically, so a placeholder byte slice is fine.
func txFixture(t *testing.T, mutate func(*std.Tx)) string {
	t.Helper()
	from := crypto.MustAddressFromString("g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5")
	to := crypto.MustAddressFromString("g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq")
	tx := std.Tx{
		Msgs: []std.Msg{bank.MsgSend{
			FromAddress: from,
			ToAddress:   to,
			Amount:      std.MustParseCoins("250000ugnot"),
		}},
		Fee:        std.NewFee(100000, std.MustParseCoin("1000000ugnot")),
		Signatures: []std.Signature{{Signature: []byte("sig")}},
		Memo:       "",
	}
	if mutate != nil {
		mutate(&tx)
	}
	bz, err := amino.Marshal(tx)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(bz)
}

func reqFixture() PaymentRequirements {
	return PaymentRequirements{
		Scheme:  "exact",
		Network: "gno:dev",
		Amount:  "250000",
		Asset:   "ugnot",
		PayTo:   "g1u7y667z64x2h7vc6fmpcprgey4ck233jaww9zq",
	}
}

// TestReasonVocabulary pins the shape of the reason vocabulary. Scheme-level
// codes carry the invalid_exact_<caip2-namespace> prefix the peer schemes use —
// "gno" is the CAIP-2 namespace, matching how SVM's constants say "solana" —
// while envelope-level codes are the spec's own §9 names and carry no scheme
// prefix. Reason strings are the one part of the wire that cannot be changed
// once integrators depend on them, so a collision or a stray prefix must fail
// here rather than after publication.
func TestReasonVocabulary(t *testing.T) {
	schemeLevel := []string{
		ReasonMalformedTransaction,
		ReasonUnexpectedMessage,
		ReasonSignatureCount,
		ReasonRecipientMismatch,
		ReasonAmountMismatch,
		ReasonMemoMismatch,
		ReasonChainMismatch,
		ReasonSimulationFailed,
		ReasonBroadcastFailed,
		ReasonSignatureInvalid,
		ReasonSequenceMismatch,
	}
	envelopeLevel := []string{
		ReasonInvalidPayload,
		ReasonInvalidVersion,
		ReasonInvalidRequirements,
		ReasonUnsupportedScheme,
		ReasonUnexpectedSettleError,
	}

	for _, reason := range schemeLevel {
		assert.True(t, strings.HasPrefix(reason, "invalid_exact_gno_"),
			"scheme-level reason %q must name the scheme and the CAIP-2 namespace", reason)
	}
	for _, reason := range envelopeLevel {
		assert.False(t, strings.HasPrefix(reason, "invalid_exact_"),
			"envelope-level reason %q is not scheme-specific", reason)
	}

	seen := make(map[string]bool)
	for _, reason := range append(append([]string{}, schemeLevel...), envelopeLevel...) {
		require.NotEmpty(t, reason)
		assert.False(t, seen[reason], "two conditions must not report the same reason: %q", reason)
		seen[reason] = true
	}
}

// TestVerifyStatic_SignatureCount separates "wrong number of signatures" from
// "could not be decoded". A transaction bearing zero or two signatures decodes
// perfectly well; reporting it as undecodable told the payer to look in the
// wrong place.
func TestVerifyStatic_SignatureCount(t *testing.T) {
	cases := map[string]func(*std.Tx){
		"none": func(tx *std.Tx) { tx.Signatures = nil },
		"two": func(tx *std.Tx) {
			tx.Signatures = append(tx.Signatures, std.Signature{Signature: []byte("sig2")})
		},
		"three": func(tx *std.Tx) {
			tx.Signatures = append(tx.Signatures, std.Signature{Signature: []byte("sig2")}, std.Signature{Signature: []byte("sig3")})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tx, _, reason := VerifyStatic(reqFixture(), SchemePayload{Transaction: txFixture(t, mutate)})
			assert.Equal(t, ReasonSignatureCount, reason)
			assert.Nil(t, tx)
		})
	}
}

// TestVerifyStatic_SubKeyCount refuses a multisig key before its subkeys are
// verified. Capping the signature count at one bounds nothing on its own: one
// signature may carry a threshold key holding thousands of subkeys, and
// verifying it costs an ed25519 check per subkey. The chain counts subkeys in
// its very first ante check, before any account read, and this check exists to
// keep that ordering — the whole reason static verification runs ahead of
// simulation is to refuse cheaply.
func TestVerifyStatic_SubKeyCount(t *testing.T) {
	subKeys := make([]crypto.PubKey, 8)
	for i := range subKeys {
		subKeys[i] = ed25519.GenPrivKey().PubKey()
	}
	multi := multisig.NewPubKeyMultisigThreshold(1, subKeys)

	tx, _, reason := VerifyStatic(reqFixture(), SchemePayload{Transaction: txFixture(t, func(tx *std.Tx) {
		tx.Signatures[0].PubKey = multi
	})})
	assert.Equal(t, ReasonSignatureCount, reason)
	assert.Nil(t, tx)
}

// TestVerifyStatic_ThresholdKeyBelowItsOwnSubKeyCount refuses a threshold key
// whose subkeys number one, which the subkey cap alone lets through:
// std.CountSubKeys sums its subkeys and answers 1, the same as an ordinary key.
//
// The count is not the only thing wrong with such a key. A threshold of zero is
// unconstructible through multisig.NewPubKeyMultisigThreshold, which panics on
// it, but amino decodes the struct whatever its fields say — and the verifier
// bounds its signature list against that threshold, so a zero one admits a
// signature list holding nothing while the bit array claims a signature is set.
// A key shape the chain's own constructor refuses has no business reaching
// verification, so the cap refuses the type outright.
func TestVerifyStatic_ThresholdKeyBelowItsOwnSubKeyCount(t *testing.T) {
	multi := multisig.PubKeyMultisigThreshold{K: 0, PubKeys: []crypto.PubKey{ed25519.GenPrivKey().PubKey()}}
	require.Equal(t, 1, std.CountSubKeys(multi), "the cap cannot rest on the subkey count for this shape")

	tx, _, reason := VerifyStatic(reqFixture(), SchemePayload{Transaction: txFixture(t, func(tx *std.Tx) {
		tx.Signatures[0].PubKey = multi
	})})
	assert.Equal(t, ReasonSignatureCount, reason)
	assert.Nil(t, tx)
}

// TestVerifyStatic_SingleKeyAndOmittedKeyAccepted pins the two shapes the
// subkey cap must not refuse: an ordinary single key, and no key at all — a
// signature omits its key when the account already stores one, and
// std.CountSubKeys reports 1 for a nil key precisely so that case still works.
func TestVerifyStatic_SingleKeyAndOmittedKeyAccepted(t *testing.T) {
	cases := map[string]crypto.PubKey{
		"single key": ed25519.GenPrivKey().PubKey(),
		"no key":     nil,
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			tx, _, reason := VerifyStatic(reqFixture(), SchemePayload{Transaction: txFixture(t, func(tx *std.Tx) {
				tx.Signatures[0].PubKey = key
			})})
			assert.Empty(t, reason)
			assert.NotNil(t, tx)
		})
	}
}

// TestValidChainID pins the rule the network name rests on. A network is
// "gno:<chain-id>" built by concatenation, and CAIP-2 names exactly two
// colon-separated parts — so a chain id carrying one produces a network string
// that reads as three. Upstream's own parser refuses it and the JS buyer returns
// null for it, which means every payment for such a facilitator is refused with
// no indication that the configuration is what is wrong.
func TestValidChainID(t *testing.T) {
	valid := []string{"dev", "test14", "portal-loop", "staging.gno.land"}
	for _, chainID := range valid {
		t.Run(chainID, func(t *testing.T) {
			assert.NoError(t, ValidChainID(chainID))
		})
	}

	invalid := map[string]string{
		"empty":            "",
		"a colon":          "test:14",
		"a leading colon":  ":dev",
		"a trailing colon": "dev:",
		"only a colon":     ":",
	}
	for name, chainID := range invalid {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, ValidChainID(chainID))
		})
	}
}

func TestVerifyStatic_ValidTx(t *testing.T) {
	tx, payer, reason := VerifyStatic(reqFixture(), SchemePayload{Transaction: txFixture(t, nil)})
	assert.Empty(t, reason)
	require.NotNil(t, tx)
	assert.Equal(t, "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5", payer)
}

func TestVerifyStatic_Rejections(t *testing.T) {
	to := crypto.MustAddressFromString("g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5") // wrong recipient

	cases := []struct {
		name   string
		req    PaymentRequirements
		txB64  string
		reason string
	}{
		{"garbage bytes", reqFixture(), "AAAA", ReasonMalformedTransaction},
		{"invalid base64", reqFixture(), "%%%", ReasonMalformedTransaction},
		{"wrong recipient", reqFixture(), txFixture(t, func(tx *std.Tx) {
			msg := tx.Msgs[0].(bank.MsgSend)
			msg.ToAddress = to
			tx.Msgs[0] = msg
		}), ReasonRecipientMismatch},
		{"wrong amount", reqFixture(), txFixture(t, func(tx *std.Tx) {
			msg := tx.Msgs[0].(bank.MsgSend)
			msg.Amount = std.MustParseCoins("1ugnot")
			tx.Msgs[0] = msg
		}), ReasonAmountMismatch},
		{"wrong denom", reqFixture(), txFixture(t, func(tx *std.Tx) {
			msg := tx.Msgs[0].(bank.MsgSend)
			msg.Amount = std.MustParseCoins("250000uatom")
			tx.Msgs[0] = msg
		}), ReasonAmountMismatch},
		{"two messages", reqFixture(), txFixture(t, func(tx *std.Tx) {
			tx.Msgs = append(tx.Msgs, tx.Msgs[0])
		}), ReasonUnexpectedMessage},
		{"non-MsgSend message", reqFixture(), txFixture(t, func(tx *std.Tx) {
			tx.Msgs = []std.Msg{vm.MsgCall{
				Caller:  crypto.MustAddressFromString("g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5"),
				PkgPath: "gno.land/r/demo/foo",
				Func:    "Bar",
			}}
		}), ReasonUnexpectedMessage},
		{"memo required but missing", func() PaymentRequirements {
			r := reqFixture()
			r.Extra = map[string]any{"memo": "pay_1"}
			return r
		}(), txFixture(t, nil), ReasonMemoMismatch},
		// A hostile requirements object must not verify as memo-less: an
		// unusable memo requirement is a broken requirements object, and
		// reporting it as satisfied would settle an unbound payment.
		{"memo present but not a string", func() PaymentRequirements {
			r := reqFixture()
			r.Extra = map[string]any{"memo": 123}
			return r
		}(), txFixture(t, nil), ReasonInvalidRequirements},
		{"memo over the cap", func() PaymentRequirements {
			r := reqFixture()
			r.Extra = map[string]any{"memo": strings.Repeat("a", maxMemoBytes+1)}
			return r
		}(), txFixture(t, func(tx *std.Tx) { tx.Memo = strings.Repeat("a", maxMemoBytes+1) }), ReasonInvalidRequirements},
		{"no signature", reqFixture(), txFixture(t, func(tx *std.Tx) {
			tx.Signatures = nil
		}), ReasonSignatureCount},
		{"bad amount in requirements", func() PaymentRequirements {
			r := reqFixture()
			r.Amount = "not-a-number"
			return r
		}(), txFixture(t, nil), ReasonAmountMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, _, reason := VerifyStatic(tc.req, SchemePayload{Transaction: tc.txB64})
			assert.Equal(t, tc.reason, reason)
			assert.Nil(t, tx)
		})
	}
}

// TestVerifyStatic_MemoBindingHoldsAcrossVerifications pins, without an HTTP
// facilitator or a node, what the memo-binding integration test proves over the
// wire: one memo-bound requirements value accepts the transaction carrying that
// memo and rejects a memo-less one. The steps share the requirements and run in
// order, so an accepted verification that left the memo requirement behind would
// surface as the second step passing.
func TestVerifyStatic_MemoBindingHoldsAcrossVerifications(t *testing.T) {
	const memo = "pay_bound"
	req := reqFixture()
	req.Extra = map[string]any{"memo": memo}

	steps := []struct {
		name   string
		txB64  string
		reason string
	}{
		{"bound memo accepted", txFixture(t, func(tx *std.Tx) { tx.Memo = memo }), ""},
		{"memo-less rejected", txFixture(t, nil), ReasonMemoMismatch},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			tx, _, reason := VerifyStatic(req, SchemePayload{Transaction: st.txB64})
			require.Equal(t, st.reason, reason)
			if st.reason == "" {
				require.NotNil(t, tx)
				return
			}
			require.Nil(t, tx)
		})
	}
}

func TestVerifyStatic_MemoMatchAccepted(t *testing.T) {
	req := reqFixture()
	req.Extra = map[string]any{"memo": "pay_1"}
	tx, _, reason := VerifyStatic(req, SchemePayload{Transaction: txFixture(t, func(tx *std.Tx) { tx.Memo = "pay_1" })})
	assert.Empty(t, reason)
	assert.NotNil(t, tx)
}
