package kimi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestListModelsParsesKimiShape parses the documented GET /models body: an
// object/list whose entries carry id, object, created, owned_by,
// context_length, and the capability flags
// (third_party/kimi-docs/api/list-models.md).
func TestListModelsParsesKimiShape(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models against base URL %q", r.URL.Path, srv.URL)
		}
		fmt.Fprintln(w, `{"object":"list","data":[
			{"id":"kimi-k3","object":"model","created":1700000000,"owned_by":"moonshot","context_length":1048576,"supports_image_in":true,"supports_video_in":true,"supports_reasoning":true},
			{"id":"kimi-k2.6","object":"model","created":1700000001,"owned_by":"moonshot","context_length":262144,"supports_image_in":false,"supports_video_in":false,"supports_reasoning":true}
		]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	resp, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("got %d models, want 2", len(resp.Data))
	}
	k3 := resp.Data[0]
	if k3.ID != "kimi-k3" || k3.OwnedBy != "moonshot" || k3.ContextLength != 1048576 || !k3.SupportsImageIn || !k3.SupportsReasoning {
		t.Errorf("kimi-k3 entry = %+v, want id kimi-k3 owned_by moonshot context 1048576 with image+reasoning", k3)
	}
}

// TestGetBalanceParsesKimiShape parses the documented GET /users/me/balance
// body — code/data/scode/status with the USD balances in data
// (third_party/kimi-docs/api/balance.md, openapi.json BalanceResponse).
func TestGetBalanceParsesKimiShape(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me/balance" {
			t.Errorf("path = %q, want /users/me/balance against base URL %q", r.URL.Path, srv.URL)
		}
		fmt.Fprintln(w, `{"code":0,"data":{"available_balance":49.58894,"voucher_balance":46.58893,"cash_balance":3.00001},"scode":"0x0","status":true}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	resp, err := c.GetBalance(context.Background())
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}
	if resp.Code != 0 || resp.Scode != "0x0" || !resp.Status {
		t.Errorf("envelope = code %d scode %q status %v, want 0/0x0/true", resp.Code, resp.Scode, resp.Status)
	}
	if resp.Data.AvailableBalance != 49.58894 || resp.Data.VoucherBalance != 46.58893 || resp.Data.CashBalance != 3.00001 {
		t.Errorf("balances = %+v, want available 49.58894 voucher 46.58893 cash 3.00001", resp.Data)
	}
}

// TestEndpointsRejectEmptyKey pins the empty-key contract on the auxiliary
// endpoints too, before any bytes are sent.
func TestEndpointsRejectEmptyKey(t *testing.T) {
	c := NewClient("http://unused.invalid", "", WithAPIKeyProvider(func() (string, error) {
		return "", nil
	}))
	if _, err := c.GetBalance(context.Background()); err != ErrNoAPIKey {
		t.Errorf("GetBalance error = %v, want ErrNoAPIKey", err)
	}
}
