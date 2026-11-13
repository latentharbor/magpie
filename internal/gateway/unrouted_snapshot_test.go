package gateway

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestUnroutedTimerKeepsItsOwnCaller(t *testing.T) {
	unroutedLimits(t, time.Hour, time.Hour)
	unknownLimit(t, time.Hour)
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Magpie-Agent", "original")
	f := &inflight{start: time.Now()}
	s := &Server{}
	_, request, stop := s.watchUnrouted(httptest.NewRecorder(), r, f)
	defer stop()
	if f.unrouted == nil {
		t.Fatal("request was not watched")
	}
	request.Header.Set("X-Magpie-Agent", "changed")
	request.URL.Path = "/changed"
	if f.unrouted.r.Header.Get("X-Magpie-Agent") != "original" || f.unrouted.r.URL.Path != "/v1/messages" {
		t.Fatal("handler mutations reached the timer's caller snapshot")
	}
}
