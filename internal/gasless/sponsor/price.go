package sponsor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// FixedPrice is a configured XLM/USD price. For testnet, where no market
// price exists, and for tests.
type FixedPrice float64

func (p FixedPrice) XLMUSD(context.Context) (float64, error) { return float64(p), nil }

// StellarExpertPrice reads XLM/USD from the StellarExpert API, caching it for
// TTL. A stale cached price is used for up to MaxAge if a refresh fails.
type StellarExpertPrice struct {
	URL    string // e.g. https://api.stellar.expert/explorer/public/asset/XLM
	APIKey string // optional; sent as a bearer token for production quotas
	TTL    time.Duration
	MaxAge time.Duration
	Client *http.Client

	mu      sync.Mutex
	price   float64
	fetched time.Time
}

func (s *StellarExpertPrice) XLMUSD(ctx context.Context) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.price > 0 && time.Since(s.fetched) < s.TTL {
		return s.price, nil
	}
	p, err := s.fetch(ctx)
	if err == nil {
		s.price, s.fetched = p, time.Now()
		return p, nil
	}
	if s.price > 0 && time.Since(s.fetched) < s.MaxAge {
		return s.price, nil
	}
	return 0, err
}

func (s *StellarExpertPrice) fetch(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return 0, err
	}
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("stellar.expert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("stellar.expert: status %d", resp.StatusCode)
	}
	var body struct {
		Price *float64 `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("stellar.expert: decode: %w", err)
	}
	if body.Price == nil || *body.Price <= 0 {
		return 0, errors.New("stellar.expert: no XLM price")
	}
	return *body.Price, nil
}

// Quote is what latch-api needs to build a forward() call the user signs.
type Quote struct {
	FeeForwarder string `json:"fee_forwarder"`
	// Relayer is the executor address to pass as forward()'s relayer
	// argument. This service rewrites it anyway.
	Relayer  string `json:"relayer"`
	FeeToken string `json:"fee_token"`
	Symbol   string `json:"symbol"`
	// MaxFeeAmount is the max_fee_amount to sign, in the token's units (7
	// decimals). It covers the highest inclusion fee this service will bid,
	// so a later surge can't push the real fee above it. The user is charged
	// the actual cost, usually much less.
	MaxFeeAmount int64 `json:"max_fee_amount"`
}

// Quote prices the most a forward() whose simulation reported resourceFee
// stroops can cost, in token.
func (s *Submitter) Quote(ctx context.Context, token string, resourceFee int64) (Quote, error) {
	f := s.Forward
	if f == nil {
		return Quote{}, ErrForwardNotBuilt
	}
	ft, ok := f.Tokens[token]
	if !ok {
		return Quote{}, invalid("fee_token %s is not an accepted fee token", token)
	}
	if resourceFee < 0 {
		return Quote{}, invalid("resource_fee_stroops must not be negative")
	}
	maxFee, err := f.FeeInToken(ctx, ft, s.maxBid(resourceFee))
	if err != nil {
		return Quote{}, err
	}
	return Quote{
		FeeForwarder: f.ForwarderID,
		Relayer:      f.Executor.Address(),
		FeeToken:     ft.Contract,
		Symbol:       ft.Symbol,
		MaxFeeAmount: maxFee,
	}, nil
}

// FeeTokens lists the accepted fee tokens.
func (s *Submitter) FeeTokens() []FeeToken {
	if s.Forward == nil {
		return nil
	}
	out := make([]FeeToken, 0, len(s.Forward.Tokens))
	for _, t := range s.Forward.Tokens {
		out = append(out, t)
	}
	return out
}

// ForwardAddresses returns the FeeForwarder and the executor (forward()'s
// relayer argument), or empty strings when forward mode is off.
func (s *Submitter) ForwardAddresses() (feeForwarder, relayer string) {
	if s.Forward == nil {
		return "", ""
	}
	return s.Forward.ForwarderID, s.Forward.Executor.Address()
}
