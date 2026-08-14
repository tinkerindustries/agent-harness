package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestParsePaging(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		def, max   int
		wantLimit  int
		wantOffset int
	}{
		{"absent", "", 20, 200, 20, 0},
		{"explicit", "?limit=50&offset=40", 20, 200, 50, 40},
		{"zero limit falls back", "?limit=0", 20, 200, 20, 0},
		{"negative limit falls back", "?limit=-5", 20, 200, 20, 0},
		{"over max falls back", "?limit=5000", 20, 200, 20, 0},
		{"non-numeric limit falls back", "?limit=abc", 20, 200, 20, 0},
		{"negative offset clamps to zero", "?offset=-10", 20, 200, 20, 0},
		{"non-numeric offset defaults", "?offset=xyz", 20, 200, 20, 0},
		{"max exactly is kept", "?limit=200", 20, 200, 200, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/sessions"+tc.query, nil)
			limit, offset := parsePaging(req, tc.def, tc.max)
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Fatalf("parsePaging(%q) = (%d, %d), want (%d, %d)",
					tc.query, limit, offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestNewPageHasMoreArithmetic(t *testing.T) {
	// 25 rows, page size 20: the first page is full and has more, and next
	// is the offset of the row after the page.
	items := make([]int, 20)
	page := newPage(items, 25, 20, 0)
	if !page.HasMore {
		t.Fatal("expected has_more on a full first page")
	}
	if page.Next == nil || *page.Next != 20 {
		t.Fatalf("expected next 20, got %v", page.Next)
	}
	if len(page.Items) != 20 || page.Total != 25 || page.Limit != 20 || page.Offset != 0 {
		t.Fatalf("unexpected first page: %+v", page)
	}

	// The last page: 5 rows, exactly to the end — no more, and no next.
	page = newPage([]int{1, 2, 3, 4, 5}, 25, 20, 20)
	if page.HasMore {
		t.Fatal("expected has_more false on the last page")
	}
	if page.Next != nil {
		t.Fatalf("expected nil next on the last page, got %d", *page.Next)
	}

	// A page that ends exactly on the total (25 rows, page 5 of 5): no more.
	page = newPage(make([]int, 5), 25, 5, 20)
	if page.HasMore {
		t.Fatal("expected has_more false when the page ends exactly on the total")
	}
	if page.Next != nil {
		t.Fatalf("expected nil next at the exact end, got %d", *page.Next)
	}
}

func TestNewPageOffsetBeyondTotal(t *testing.T) {
	// An offset past the end returns an empty items with has_more false — the
	// shape a client that kept paging after the last page must be able to
	// handle, and the same shape a shrink-then-delete race produces.
	page := newPage([]int{}, 25, 20, 100)
	if page.HasMore {
		t.Fatal("expected has_more false past the end")
	}
	if page.Next != nil {
		t.Fatalf("expected nil next past the end, got %d", *page.Next)
	}
	if page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("expected empty items past the end, got %+v", page.Items)
	}
}

func TestNewPageItemsMarshalsAsArrayNotNull(t *testing.T) {
	// newPage's guarantee that an empty page is [] on the wire, never null —
	// the exact shape a client's items.map() depends on.
	page := newPage[struct{ ID string }](nil, 0, 20, 0)
	b, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"items":[]`)) {
		t.Fatalf("expected items to marshal as [], got %s", b)
	}
}
