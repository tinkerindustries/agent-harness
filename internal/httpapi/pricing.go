package httpapi

import (
	"net/http"
	"sort"
)

// pricingResponse is GET /api/pricing: what a screen needs to say when the
// expensive hours are, and nothing more.
//
// The rates are deliberately absent. No screen prices anything — every cost
// figure the UI shows was computed when its usage event was committed and
// stored on it (docs/DESIGN.md §4.9), which is what keeps a historical
// figure stable — so a browser holding a rate card could only ever use it to
// compute a second, disagreeing number.
//
// What it does carry is the schedule, because that is a fact about the
// future: which hours will bill at the peak rate, and from when. The windows
// go out in UTC, as DeepSeek states them, and the browser renders them in
// its own zone. That is the right split — the server has no idea where the
// person reading is sitting, and the browser knows exactly.
type pricingResponse struct {
	CapturedAt string           `json:"captured_at"`
	Schedule   *pricingSchedule `json:"schedule,omitempty"`
}

type pricingSchedule struct {
	EffectiveAt    string          `json:"effective_at"`
	PeakWindowsUTC []pricingWindow `json:"peak_windows_utc"`
	// Models the schedule prices by the hour. A model absent from this list
	// bills one rate around the clock, so a screen showing a Gemini or Kimi
	// session must not tell its reader the hour matters.
	Models []string `json:"models"`
}

type pricingWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	resp := pricingResponse{CapturedAt: s.PriceTableDate}
	if s.Prices == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if resp.CapturedAt == "" {
		resp.CapturedAt = s.Prices.CapturedAt
	}
	sched := s.Prices.Schedule
	if sched == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	out := &pricingSchedule{EffectiveAt: sched.EffectiveAt.UTC().Format("2006-01-02T15:04:05Z")}
	for _, win := range sched.PeakWindowsUTC {
		out.PeakWindowsUTC = append(out.PeakWindowsUTC, pricingWindow{From: win.From, To: win.To})
	}
	for model := range sched.Peak {
		out.Models = append(out.Models, model)
	}
	// Map iteration order is random and this response is compared by at
	// least one test and cached by the browser; sorting makes it stable.
	sort.Strings(out.Models)
	resp.Schedule = out
	writeJSON(w, http.StatusOK, resp)
}
