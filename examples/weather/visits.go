package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"
)

// A visit is the page open in one browser tab. The page puts a token naming it
// in the commands it shows, so a buyer paying from a terminal pays a URL that
// carries it, and the page finds the payment by the token.
const (
	// visitTTL is how long a payment stays findable by its visit.
	visitTTL = 15 * time.Minute
	// maxVisits bounds how many payments the seller keeps at once.
	maxVisits = 1000
	// maxVisitBody bounds the response kept with each; the weather is a few
	// dozen bytes, and a longer response is kept without its body.
	maxVisitBody = 4 << 10
	// The lookups of every page together: room for about 150 pages asking
	// every 3 s.
	lookupRatePerSecond = 50
	lookupRateBurst     = 100
)

// visitToken is a token as the page makes one: 128 random bits in hex.
var visitToken = regexp.MustCompile(`^[0-9a-f]{32}$`)

// visit is a payment settled under a visit token, with the response it bought.
type visit struct {
	Transaction string `json:"transaction"`
	Network     string `json:"network"`
	Payer       string `json:"payer"`
	Body        string `json:"body"`
	at          time.Time
}

// visitBook keeps the payments settled under a visit token for visitTTL, and
// at most maxVisits of them.
type visitBook struct {
	now func() time.Time

	mu     sync.Mutex
	visits map[string]visit
}

func newVisitBook() *visitBook {
	return &visitBook{now: time.Now, visits: make(map[string]visit)}
}

// record keeps v under token unless a payment is kept there already: the page
// shows the one that settled first. A full book forgets the payments past
// their time, then the oldest.
func (b *visitBook) record(token string, v visit) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if kept, ok := b.visits[token]; ok && now.Sub(kept.at) < visitTTL {
		return
	}
	if len(b.visits) >= maxVisits {
		// ceiling: a sweep of the whole book per payment while it is full; a queue by age if maxVisits grows
		oldest := ""
		for other, kept := range b.visits {
			switch {
			case now.Sub(kept.at) >= visitTTL:
				delete(b.visits, other)
			case oldest == "" || kept.at.Before(b.visits[oldest].at):
				oldest = other
			}
		}
		if len(b.visits) >= maxVisits {
			delete(b.visits, oldest)
		}
	}
	v.at = now
	b.visits[token] = v
}

// lookup returns the payment kept under token, if one settled there within
// visitTTL.
func (b *visitBook) lookup(token string) (visit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.visits[token]
	if !ok || b.now().Sub(v.at) >= visitTTL {
		return visit{}, false
	}
	return v, true
}

// recordSettled serves next, and keeps a payment it settled under the visit
// token the request's query names.
func (b *visitBook) recordSettled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("visit")
		if !visitToken.MatchString(token) {
			next.ServeHTTP(w, r)
			return
		}
		served := &servedResponse{ResponseWriter: w}
		next.ServeHTTP(served, r)
		if v, ok := served.settled(); ok {
			b.record(token, v)
		}
	})
}

// lookupHandler answers GET <weatherPath>/visits/{token} with the payment kept
// under the token, or 404 while there is none. The page asks again until one
// settles, so no answer may be cached.
func (b *visitBook) lookupHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		v, ok := b.lookup(r.PathValue("token"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	})
}

// servedResponse passes a response through, remembering its status and up to
// one byte more of its body than a visit keeps.
type servedResponse struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (s *servedResponse) WriteHeader(status int) {
	if s.status == 0 {
		s.status = status
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *servedResponse) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if room := maxVisitBody + 1 - s.body.Len(); room > 0 {
		s.body.Write(p[:min(len(p), room)])
	}
	return s.ResponseWriter.Write(p)
}

func (s *servedResponse) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// settled returns the payment the response's PAYMENT-RESPONSE reports settled,
// with the body it served. A refusal or a failed settlement keeps nothing.
func (s *servedResponse) settled() (visit, bool) {
	if s.status != http.StatusOK {
		return visit{}, false
	}
	raw, err := base64.StdEncoding.DecodeString(s.Header().Get("PAYMENT-RESPONSE"))
	if err != nil {
		return visit{}, false
	}
	var settle x402.SettleResponse
	if json.Unmarshal(raw, &settle) != nil || !settle.Success || settle.Transaction == "" {
		return visit{}, false
	}
	v := visit{Transaction: settle.Transaction, Network: string(settle.Network), Payer: settle.Payer}
	if s.body.Len() <= maxVisitBody {
		v.Body = s.body.String()
	}
	return v, true
}
