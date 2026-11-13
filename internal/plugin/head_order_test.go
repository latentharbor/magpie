package plugin

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

// Force the exact boundary: the consumer observed an empty queue, then the
// producer queued the head/body and published completion before its next read.
func TestHostCompletedHeadAfterEmptyQueueCheck(t *testing.T) {
	for _, why := range []string{"", "vendor ended with an error"} {
		t.Run(why, func(t *testing.T) {
			f := newFakeHost(t)
			id, c := f.begin(t, true)
			if _, ok := c.pop(); ok {
				t.Fatal("new call already had a head")
			}
			if !c.push(message{Event: "head", Status: 200, Headers: map[string]string{"x-test": "kept"}}) ||
				!c.push(message{Event: "chunk", Data: base64.StdEncoding.EncodeToString([]byte("body"))}) {
				t.Fatal("producer could not queue its response")
			}
			terminal := message{}
			if why != "" {
				terminal = errAnswer(why)
			}
			c.transition(callAnswered, nil, &terminal)
			head, answer, err := c.completedHead(c.result())
			if err != nil || head.Status != 200 || head.Headers["x-test"] != "kept" {
				t.Fatalf("queued head lost after empty observation: %+v %v", head, err)
			}
			b := &body{h: f.host, id: id, c: c, ctx: context.Background(), answer: answer, closed: make(chan struct{})}
			defer b.Close()
			data, err := io.ReadAll(b)
			if string(data) != "body" || (why == "" && err != nil) || (why != "" && (err == nil || !strings.Contains(err.Error(), why))) {
				t.Fatalf("body or terminal changed: %q %v", data, err)
			}
		})
	}
}

func TestHostCompletionWithoutHeadStillFails(t *testing.T) {
	for _, why := range []string{"", "vendor refused"} {
		c := &call{}
		answer := message{}
		if why != "" {
			answer = errAnswer(why)
		}
		_, kept, err := c.completedHead(&answer)
		if err == nil || kept != &answer || (why != "" && !strings.Contains(err.Error(), why)) {
			t.Fatalf("missing response was accepted or changed: %v %v", kept, err)
		}
	}
}
