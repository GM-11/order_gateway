package auth

import (
	"net/http/httptest"
	"testing"
)

func TestFromRequest(t *testing.T) {
	tests := []struct {
		name string
		set  func(*httptest.ResponseRecorder)
		want Credentials
	}{
		{name: "API key headers only", set: func(w *httptest.ResponseRecorder) {
			w.Header().Set("API_KEY_ID", "key-a")
			w.Header().Set("API_SECRET", "secret-a")
		}, want: Credentials{KeyID: "key-a", Secret: "secret-a"}},
		{name: "Bearer header only", set: func(w *httptest.ResponseRecorder) {
			w.Header().Set("Authorization", "Bearer token-a")
		}, want: Credentials{Bearer: "Bearer token-a"}},
		{name: "lowercase bearer", set: func(w *httptest.ResponseRecorder) {
			w.Header().Set("Authorization", "bearer token-a")
		}, want: Credentials{}},
		{name: "Basic ignored", set: func(w *httptest.ResponseRecorder) {
			w.Header().Set("Authorization", "Basic xyz")
		}, want: Credentials{}},
		{name: "no headers", set: func(*httptest.ResponseRecorder) {}, want: Credentials{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.set(w)
			req := httptest.NewRequest("GET", "/", nil)
			req.Header = w.Header()
			if got := FromRequest(req); got != tt.want {
				t.Errorf("FromRequest() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
