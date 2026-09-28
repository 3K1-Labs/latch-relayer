package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/strkey"
)

// sessionFile keeps the rotating refresh token between runs so the emailed OTP
// is only typed once per refresh-token lifetime (30 days). The *.env rule in
// .gitignore keeps it out of git.
const sessionFile = ".e2e-session.env"

// apiClient speaks latch-api's envelope: {"data": ...} on success,
// {"error": {"code", "message"}} on failure.
type apiClient struct {
	base  string
	token string
	http  *http.Client
}

func newAPIClient(base, token string) *apiClient {
	// Generous: a sleeping Render instance takes ~15-30s to wake, and a
	// smart-account deploy waits for ledger inclusion.
	return &apiClient{base: base, token: token, http: &http.Client{Timeout: 90 * time.Second}}
}

// call sends one request and, on a 2xx, decodes the envelope's data into out.
// It returns the status and raw body so callers can assert on failures too.
func (c *apiClient) call(method, path string, body, out any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if out != nil && resp.StatusCode/100 == 2 {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) == 0 {
			return resp.StatusCode, raw, fmt.Errorf("%s %s: no data in response: %s", method, path, trim(raw))
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("%s %s: decode data: %w", method, path, err)
		}
	}
	return resp.StatusCode, raw, nil
}

// must is call for steps that have to succeed: anything but a 2xx is an error.
func (c *apiClient) must(method, path string, body, out any) (int, error) {
	status, raw, err := c.call(method, path, body, out)
	if err != nil {
		return status, err
	}
	if status/100 != 2 {
		return status, fmt.Errorf("%s %s: HTTP %d: %s", method, path, status, trim(raw))
	}
	return status, nil
}

// ── Auth ─────────────────────────────────────────────────────────────────────

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// emailLogin returns an access token for email. It reuses a saved refresh
// token when it still works, and otherwise runs the OTP flow, asking for the
// emailed code on stdin.
func emailLogin(api *apiClient, email string, stdin *bufio.Reader) (string, error) {
	saved, err := godotenv.Read(sessionFile)
	if err != nil {
		saved = map[string]string{}
	}
	key := sessionKey(email)

	if rt := saved[key]; rt != "" {
		var t tokenPair
		if _, err := api.must(http.MethodPost, "/v1/auth/refresh", map[string]string{"refresh_token": rt}, &t); err == nil && t.AccessToken != "" {
			step("logged in as %s (saved session)", email)
			return t.AccessToken, saveSession(saved, key, t.RefreshToken)
		}
		step("saved session for %s no longer valid, falling back to OTP", email)
	}

	// E2E_OTP answers a code a previous run already requested; asking for a new
	// one would invalidate it (and there are only 3 per hour).
	otp := os.Getenv("E2E_OTP")
	if otp == "" {
		if _, err := api.must(http.MethodPost, "/v1/auth/register", map[string]string{"email": email}, nil); err != nil {
			return "", err
		}
		fmt.Printf("  ? enter the code emailed to %s: ", email)
		line, err := stdin.ReadString('\n')
		if strings.TrimSpace(line) == "" {
			// No one at the keyboard (CI, /dev/null): the code is on its way,
			// so hand it back through the environment on the next run.
			fatalf("a login code was emailed to %s; rerun with E2E_OTP=<code> (%v)", email, err)
		}
		otp = line
	}
	var t tokenPair
	if _, err := api.must(http.MethodPost, "/v1/auth/verify", map[string]string{"email": email, "otp": strings.TrimSpace(otp)}, &t); err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", fmt.Errorf("verify returned no access_token")
	}
	step("logged in as %s (OTP)", email)
	return t.AccessToken, saveSession(saved, key, t.RefreshToken)
}

func saveSession(saved map[string]string, key, refreshToken string) error {
	// Refresh rotates the token; keep the old one only if none came back.
	if refreshToken == "" {
		return nil
	}
	saved[key] = refreshToken
	if err := godotenv.Write(saved, sessionFile); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return os.Chmod(sessionFile, 0o600)
}

// sessionKey names an email's entry without writing the address itself to disk.
func sessionKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	return "E2E_REFRESH_" + hex.EncodeToString(sum[:6])
}

// walletLogin signs latch-api's wallet challenge with kp. Wallet-scope tokens
// cannot use /v1/accounts today; the walletauth scenario tracks that.
func walletLogin(api *apiClient, kp *keypair.Full) (string, error) {
	req := map[string]string{"wallet": kp.Address(), "key_type": "ed25519", "network": "testnet"}
	var ch struct {
		Nonce string `json:"nonce"`
	}
	if _, err := api.must(http.MethodPost, "/v1/auth/challenge", req, &ch); err != nil {
		return "", err
	}
	// The sign-in nonce is base64url; the signature covers its raw bytes.
	nonce, err := base64.RawURLEncoding.DecodeString(ch.Nonce)
	if err != nil {
		return "", fmt.Errorf("decode nonce: %w", err)
	}
	sig, err := kp.Sign(nonce)
	if err != nil {
		return "", err
	}
	req["nonce"] = ch.Nonce
	req["signature"] = base64.StdEncoding.EncodeToString(sig)
	var t tokenPair
	if _, err := api.must(http.MethodPost, "/v1/auth/sign-in", req, &t); err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// deploySmartAccount deploys the deterministic ed25519 smart account for
// signer through latch-api (the bundler pays), proving possession of the key
// over a single-use nonce.
func deploySmartAccount(api *apiClient, signer *keypair.Full) (string, error) {
	pub, err := strkey.Decode(strkey.VersionByteAccountID, signer.Address())
	if err != nil {
		return "", err
	}
	pubHex := hex.EncodeToString(pub)

	var ch struct {
		Nonce string `json:"nonce"`
	}
	challenge := map[string]string{"key_type": "ed25519", "key_ref": pubHex, "network": "testnet"}
	if _, err := api.must(http.MethodPost, "/v1/smart-account/challenge", challenge, &ch); err != nil {
		return "", err
	}
	// The deploy nonce is hex; the signature covers its decoded bytes.
	nonce, err := hex.DecodeString(ch.Nonce)
	if err != nil {
		return "", fmt.Errorf("decode deploy nonce: %w", err)
	}
	sig, err := signer.Sign(nonce)
	if err != nil {
		return "", err
	}

	var out struct {
		Address string `json:"smart_account_address"`
	}
	body := map[string]any{
		"public_key_hex": pubHex,
		"network":        "testnet",
		"proof":          map[string]string{"nonce": ch.Nonce, "signature": base64.StdEncoding.EncodeToString(sig)},
	}
	if _, err := api.must(http.MethodPost, "/v1/smart-account/ed25519", body, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.Address, "C") {
		return "", fmt.Errorf("deploy returned %q, not a C-address", out.Address)
	}
	return out.Address, nil
}

// ── Deposit intents ──────────────────────────────────────────────────────────

type intentRequest struct {
	SmartAccountAddress string `json:"smart_account_address"`
	Network             string `json:"network,omitempty"`
	ExpiresIn           int    `json:"expires_in,omitempty"`
	ExpectedAmt         string `json:"expected_amt,omitempty"`
	ExternalID          string `json:"external_id,omitempty"`
}

type intent struct {
	IntentID    string `json:"intent_id"`
	MemoID      string `json:"memo_id"`
	PoolAddress string `json:"pool_address"`
	ExpiresAt   string `json:"expires_at"`
}

// createIntent opens a deposit intent. latch-api returns 503 when the relayer
// outlasts its own boot budget; the integration guide says one delayed retry
// is right, so this does exactly that.
func createIntent(api *apiClient, req intentRequest) (intent, error) {
	var in intent
	status, err := api.must(http.MethodPost, "/v1/accounts/deposit-intent", req, &in)
	if status == http.StatusServiceUnavailable {
		step("latch-api says the relayer is waking up, retrying in 10s")
		time.Sleep(10 * time.Second)
		_, err = api.must(http.MethodPost, "/v1/accounts/deposit-intent", req, &in)
	}
	if err != nil {
		return in, err
	}
	step("intent %s memo_id=%s pool=%s expires_at=%s", in.IntentID, in.MemoID, in.PoolAddress, in.ExpiresAt)
	return in, nil
}

type depositStatus struct {
	IntentID string    `json:"intent_id"`
	MemoID   string    `json:"memo_id"`
	CAddress string    `json:"c_address"`
	Status   string    `json:"status"`
	Forwards []forward `json:"forwards"`
}

type forward struct {
	TxHash    string  `json:"tx_hash"`
	Amount    string  `json:"amount"`
	Asset     string  `json:"asset"`
	Status    string  `json:"status"`
	ForwardTx *string `json:"forward_tx"`
}

func getStatus(api *apiClient, memoID string) (depositStatus, error) {
	var s depositStatus
	_, err := api.must(http.MethodGet, "/v1/accounts/deposit/status/"+memoID, nil, &s)
	return s, err
}

func trim(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
