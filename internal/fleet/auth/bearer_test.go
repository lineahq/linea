package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearerValidation(t *testing.T) {
	token := strings.Repeat("s", 32)
	b, err := New(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"", "short", token + "\n", strings.Repeat("s", 1025)} {
		if _, err := New(secret); err == nil {
			t.Fatal("accepted invalid bootstrap secret")
		}
	}
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"Bearer " + token, true}, {"bearer " + token, true}, {"Basic " + token, false},
		{"Bearer " + token + " ", false}, {"Bearer " + strings.Repeat("x", 32), false}, {"", false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", tc.header)
		if b.Valid(r) != tc.want {
			t.Fatalf("unexpected result for scheme %q", strings.Split(tc.header, " ")[0])
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Add("Authorization", "Bearer "+token)
	r.Header.Add("Authorization", "Bearer "+token)
	if b.Valid(r) {
		t.Fatal("accepted ambiguous credentials")
	}
}
