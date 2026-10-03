package adapter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// seen is what the daemon received.
type seen struct {
	path, query, auth, apiKey, host, body string
}

func daemon(t *testing.T) (*httptest.Server, *[]seen) {
	var got []seen
	s := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Host, string(body)})
		rw.Write([]byte("reply to " + r.URL.Path))
	}))
	t.Cleanup(s.Close)
	return s, &got
}

func adapter(t *testing.T, d *httptest.Server) *httptest.Server {
	target, _ := url.Parse(d.URL + "/upstream/immich-ml/")
	a := httptest.NewServer(New(target, "the-key", map[string]string{"/ping": "/health"}))
	t.Cleanup(a.Close)
	return a
}

func TestARequestGoesUnderTheTargetWithTheKeyInPlaceOfTheClients(t *testing.T) {
	d, got := daemon(t)
	a := adapter(t, d)

	req, _ := http.NewRequest(http.MethodPost, a.URL+"/predict?x=1", strings.NewReader("multipart body"))
	req.Header.Set("Authorization", "Bearer the-clients-own")
	req.Header.Set("X-Api-Key", "another")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(reply) != "reply to /upstream/immich-ml/predict" {
		t.Errorf("reply = %q", reply)
	}
	want := seen{"/upstream/immich-ml/predict", "x=1", "Bearer the-key", "", strings.TrimPrefix(d.URL, "http://"), "multipart body"}
	if len(*got) != 1 || (*got)[0] != want {
		t.Errorf("the daemon saw %+v, want %+v", *got, want)
	}
}

func TestARoutedPathGoesToItsOwnPathOnTheTargetsHost(t *testing.T) {
	d, got := daemon(t)
	a := adapter(t, d)

	resp, err := http.Get(a.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(*got) != 1 || (*got)[0].path != "/health" || (*got)[0].auth != "Bearer the-key" {
		t.Errorf("the daemon saw %+v, want /health with the key", *got)
	}
}
