package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
)

const pricingTestTable = `{
  "captured_at": "2026-08-14",
  "rate_schedule": {
    "effective_at": "2026-08-16T16:00:00Z",
    "peak_windows_utc": [{"from":"01:00","to":"04:00"},{"from":"06:00","to":"10:00"}],
    "peak": {
      "deepseek-v4-pro": {"input_cache_hit_per_million_usd":0.044,"input_cache_miss_per_million_usd":1.32,"output_per_million_usd":3.96},
      "deepseek-v4-flash": {"input_cache_hit_per_million_usd":0.014,"input_cache_miss_per_million_usd":0.44,"output_per_million_usd":1.32}
    },
    "off_peak": {
      "deepseek-v4-pro": {"input_cache_hit_per_million_usd":0.022,"input_cache_miss_per_million_usd":0.66,"output_per_million_usd":1.98},
      "deepseek-v4-flash": {"input_cache_hit_per_million_usd":0.007,"input_cache_miss_per_million_usd":0.22,"output_per_million_usd":0.66}
    }
  },
  "models": {
    "deepseek-v4-pro": {"input_cache_hit_per_million_usd":0.003625,"input_cache_miss_per_million_usd":0.435,"output_per_million_usd":0.87},
    "deepseek-v4-flash": {"input_cache_hit_per_million_usd":0.0028,"input_cache_miss_per_million_usd":0.14,"output_per_million_usd":0.28},
    "gemini-3.7-flash": {"input_cache_hit_per_million_usd":0.075,"input_cache_miss_per_million_usd":0.75,"output_per_million_usd":3.75}
  }
}`

func loadPricingTestTable(t *testing.T) *pricing.Table {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte(pricingTestTable), 0o644); err != nil {
		t.Fatal(err)
	}
	table, err := pricing.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return table
}

// staticPlaceholder stands in for the embedded frontend. The router mounts
// it at "/", and a nil handler there panics when anything is served — so
// every Server built by hand in a test needs one, whatever it is testing.
func staticPlaceholder() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "static placeholder")
	})
}

func getPricing(t *testing.T, api *Server) (int, string, pricingResponse) {
	t.Helper()
	api.Static = staticPlaceholder()
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + "/api/pricing")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body pricingResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), body
}

func TestPricingServesTheSchedule(t *testing.T) {
	api := &Server{Prices: loadPricingTestTable(t), PriceTableDate: "2026-08-14"}
	code, _, body := getPricing(t, api)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body.CapturedAt != "2026-08-14" {
		t.Errorf("captured_at = %q, want 2026-08-14", body.CapturedAt)
	}
	if body.Schedule == nil {
		t.Fatal("no schedule in the response")
	}
	if body.Schedule.EffectiveAt != "2026-08-16T16:00:00Z" {
		t.Errorf("effective_at = %q", body.Schedule.EffectiveAt)
	}
	if len(body.Schedule.PeakWindowsUTC) != 2 ||
		body.Schedule.PeakWindowsUTC[0].From != "01:00" || body.Schedule.PeakWindowsUTC[0].To != "04:00" ||
		body.Schedule.PeakWindowsUTC[1].From != "06:00" || body.Schedule.PeakWindowsUTC[1].To != "10:00" {
		t.Errorf("windows = %+v, want 01:00-04:00 and 06:00-10:00", body.Schedule.PeakWindowsUTC)
	}
	// Sorted, because map order is random and this response is cached.
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro"}
	if len(body.Schedule.Models) != len(want) {
		t.Fatalf("models = %v, want %v", body.Schedule.Models, want)
	}
	for i := range want {
		if body.Schedule.Models[i] != want[i] {
			t.Errorf("models = %v, want %v (sorted)", body.Schedule.Models, want)
			break
		}
	}
}

// TestPricingWithholdsTheRates. The browser prices nothing — every cost
// figure it shows was computed when its usage event was committed — so a
// rate card in the response could only ever be used to compute a second,
// disagreeing number. This asserts on the raw bytes because the typed
// response cannot express a field that is not on it.
func TestPricingWithholdsTheRates(t *testing.T) {
	api := &Server{Prices: loadPricingTestTable(t), PriceTableDate: "2026-08-14", Static: staticPlaceholder()}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/pricing")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw := make([]byte, 4096)
	n, _ := res.Body.Read(raw)
	text := string(raw[:n])
	for _, leak := range []string{"per_million", "0.435", "3.96", "0.0028"} {
		if strings.Contains(text, leak) {
			t.Errorf("response leaks a rate (%q): %s", leak, text)
		}
	}
}

// TestPricingWithoutATableIsEmptyNotAnError. A harness built without a price
// table still serves its screens; the schedule is simply absent and the UI
// says nothing about peak hours rather than showing an error where a hint
// belongs.
func TestPricingWithoutATableIsEmptyNotAnError(t *testing.T) {
	code, _, body := getPricing(t, &Server{})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body.Schedule != nil {
		t.Errorf("schedule = %+v, want nil", body.Schedule)
	}
}

// TestPricingWithoutAScheduleIsEmpty covers a table loaded from a config
// that has rates but no rate_schedule — every table before 2026-08-16, and
// any provider that never splits its day.
func TestPricingWithoutAScheduleIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	body := `{"captured_at":"2026-08-01","models":{"deepseek-v4-pro":{"input_cache_hit_per_million_usd":0.003625,"input_cache_miss_per_million_usd":0.435,"output_per_million_usd":0.87}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	table, err := pricing.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	code, _, got := getPricing(t, &Server{Prices: table})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got.Schedule != nil {
		t.Errorf("schedule = %+v, want nil", got.Schedule)
	}
	// Falls back to the table's own date when the server was not given one.
	if got.CapturedAt != "2026-08-01" {
		t.Errorf("captured_at = %q, want the table's own 2026-08-01", got.CapturedAt)
	}
}
