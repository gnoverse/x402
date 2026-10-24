package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gnolang/gno/tm2/pkg/std"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gnoverse/x402/facilitator"
	"github.com/gnoverse/x402/facilitator/exact/exacttest"
)

// aVisit is a token as the page makes one: 128 random bits in hex.
const aVisit = "0123456789abcdef0123456789abcdef"

// bookAt returns an empty visit book whose clock reads *now.
func bookAt(now *time.Time) *visitBook {
	b := newVisitBook()
	b.now = func() time.Time { return *now }
	return b
}

// A token copied into two terminals pays twice; the page shows the payment
// that settled first, which is the one it can still be looking at.
func TestAVisitKeepsTheFirstPaymentSettledUnderIt(t *testing.T) {
	now := time.Now()
	book := bookAt(&now)

	book.record(aVisit, visit{Transaction: "first"})
	book.record(aVisit, visit{Transaction: "second"})

	kept, found := book.lookup(aVisit)
	require.True(t, found)
	assert.Equal(t, "first", kept.Transaction)
}

func TestAVisitIsForgottenOnceItsTimeIsUp(t *testing.T) {
	now := time.Now()
	book := bookAt(&now)
	book.record(aVisit, visit{Transaction: "first"})

	now = now.Add(visitTTL)

	_, found := book.lookup(aVisit)
	assert.False(t, found)
	book.record(aVisit, visit{Transaction: "later"})
	kept, found := book.lookup(aVisit)
	require.True(t, found, "a forgotten token takes a new payment")
	assert.Equal(t, "later", kept.Transaction)
}

// The book is the seller's memory of who paid, so a flood of visits replaces
// the oldest rather than growing it.
func TestTheVisitBookForgetsItsOldestWhenFull(t *testing.T) {
	now := time.Now()
	book := bookAt(&now)
	for i := range maxVisits + 1 {
		book.record(fmt.Sprintf("%032x", i), visit{Transaction: fmt.Sprint(i)})
		now = now.Add(time.Millisecond)
	}

	_, found := book.lookup(fmt.Sprintf("%032x", 0))
	assert.False(t, found, "the oldest visit is still kept")
	_, found = book.lookup(fmt.Sprintf("%032x", maxVisits))
	assert.True(t, found, "the newest visit was dropped")
	assert.Len(t, book.visits, maxVisits)
}

// lookUpVisit asks the seller for the payment kept under token.
func lookUpVisit(t *testing.T, sellerURL, token string) *http.Response {
	t.Helper()
	resp, err := http.Get(sellerURL + "/weather/visits/" + token)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// A buyer in a terminal pays the URL the page put its visit token in, and the
// page, which saw none of it, finds the payment and the weather by the token.
func TestThePageFindsAPaymentByTheVisitItWasMadeUnder(t *testing.T) {
	txB64, account := exacttest.SignedTx(t, nil)
	gnofacilitator := httptest.NewServer(facilitator.New(&exacttest.Node{Account: account, Hash: "abc123"}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	paid := payAt(t, sellerURL, "/weather?visit="+aVisit, txB64)
	require.Equal(t, http.StatusOK, paid.StatusCode)
	served, err := io.ReadAll(paid.Body)
	require.NoError(t, err)

	found := lookUpVisit(t, sellerURL, aVisit)
	require.Equal(t, http.StatusOK, found.StatusCode)
	assert.Equal(t, "no-store", found.Header.Get("Cache-Control"))
	var kept struct{ Transaction, Network, Payer, Body string }
	require.NoError(t, json.NewDecoder(found.Body).Decode(&kept))
	assert.Equal(t, "abc123", kept.Transaction)
	assert.Equal(t, "gno:"+exacttest.ChainID, kept.Network)
	assert.Equal(t, exacttest.Payer, kept.Payer)
	assert.Equal(t, string(served), kept.Body)
}

// The page asks until a payment settles, and no answer may be cached in between.
func TestAVisitNoPaymentSettledUnderIsNotFound(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	found := lookUpVisit(t, sellerURL, aVisit)

	assert.Equal(t, http.StatusNotFound, found.StatusCode)
	assert.Equal(t, "no-store", found.Header.Get("Cache-Control"))
}

// Only a settled payment lights the page: a refused one moved nothing.
func TestARefusedPaymentIsKeptUnderNoVisit(t *testing.T) {
	txB64, account := exacttest.SignedTx(t, nil)
	node := &exacttest.Node{Account: account, SimulateErr: std.ErrInsufficientFunds("insufficient funds")}
	gnofacilitator := httptest.NewServer(facilitator.New(node, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL)

	paid := payAt(t, sellerURL, "/weather?visit="+aVisit, txB64)
	require.Equal(t, http.StatusPaymentRequired, paid.StatusCode)

	assert.Equal(t, http.StatusNotFound, lookUpVisit(t, sellerURL, aVisit).StatusCode)
}

// The priced route's limit spares the facilitator; pages polling for their
// visits cost the seller a map read, and many poll at once.
func TestLookupsAreNotThrottledByThePricedRoutesLimit(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL, func(cfg *sellerConfig) {
		cfg.ratePerSecond = 0.001
		cfg.rateBurst = 1
	})

	for range 20 {
		assert.Equal(t, http.StatusNotFound, lookUpVisit(t, sellerURL, aVisit).StatusCode)
	}
}

// Pages polling for their visits must not spend the budget that admits a
// payment, or a page left open would refuse the next buyer.
func TestLookingUpVisitsSpendsNothingOfThePricedRoutesLimit(t *testing.T) {
	gnofacilitator := httptest.NewServer(facilitator.New(unreachableChain{}, exacttest.ChainID).Handler())
	t.Cleanup(gnofacilitator.Close)
	sellerURL, _ := weatherSeller(t, gnofacilitator.URL, func(cfg *sellerConfig) {
		cfg.ratePerSecond = 0.001
		cfg.rateBurst = 1
	})

	lookUpVisit(t, sellerURL, aVisit)

	unpaid, err := http.Get(sellerURL + "/weather")
	require.NoError(t, err)
	require.NoError(t, unpaid.Body.Close())
	assert.Equal(t, http.StatusPaymentRequired, unpaid.StatusCode)
}
