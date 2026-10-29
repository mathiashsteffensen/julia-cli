package browser

import (
	"testing"

	"github.com/chromedp/cdproto/network"
)

// A request continued with headers keeps its own, except those the policy
// replaces, whatever their case.
func TestWithHeaders(t *testing.T) {
	own := network.Headers{"Accept": "text/html", "authorization": "page-own", "X-Page": "kept"}
	got := map[string]string{}
	for _, h := range withHeaders(own, map[string]string{"Authorization": "Bearer tok", "X-Who": "ada"}) {
		if _, twice := got[h.Name]; twice {
			t.Fatalf("%s sent twice", h.Name)
		}
		got[h.Name] = h.Value
	}
	want := map[string]string{"Accept": "text/html", "Authorization": "Bearer tok", "X-Page": "kept", "X-Who": "ada"}
	if len(got) != len(want) {
		t.Fatalf("headers %v, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("headers %v, want %v", got, want)
		}
	}
}
