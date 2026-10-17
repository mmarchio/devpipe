package providermodels

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func serve(t *testing.T, status int, body string, wantAuth string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("Authorization = %q, want %q", got, wantAuth)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/models"
}

func TestListOpenAIShape(t *testing.T) {
	u := serve(t, 200, `{"object":"list","data":[{"id":"b"},{"id":"a"},{"id":"b"},{"id":" "}]}`, "Bearer tok")
	got, err := List(context.Background(), http.DefaultClient, u, "tok")
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestListOllamaShape(t *testing.T) {
	u := serve(t, 200, `{"models":[{"name":"n1","model":"m1"},{"name":"n2"}]}`, "")
	got, err := List(context.Background(), http.DefaultClient, u, "")
	if err != nil || !reflect.DeepEqual(got, []string{"m1", "n2"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestListFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{401, `{"error":"no"}`}, {200, `not json`}} {
		u := serve(t, tc.status, tc.body, "")
		if _, err := List(context.Background(), http.DefaultClient, u, ""); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%d %q: err = %v", tc.status, tc.body, err)
		}
	}
}
