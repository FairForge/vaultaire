package engine

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

type statusAnswer int

func (s statusAnswer) Error() string       { return fmt.Sprintf("HTTP %d", int(s)) }
func (s statusAnswer) HTTPStatusCode() int { return int(s) }

func TestPutOutcome_SentListsEveryFailedAttemptThatWentOut(t *testing.T) {
	// An error ANSWER still counts as sent: a proxy in front of the store can
	// answer 503 after the store kept the body (Blomp, 2026-10-21). Unanswered
	// keeps its stricter meaning for the chunk store.
	o := &PutOutcome{}
	o.record("answered", statusAnswer(503))
	o.record("timeout", errors.New("no answer within 60s"))
	o.record("dial", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	o.record("dns", &net.DNSError{Err: "no such host", Name: "x"})
	o.record("refused", fmt.Errorf("bad name: %w", ErrInvalidInput))
	o.record("ok", nil)
	o.record("answered", statusAnswer(500))

	assert.Equal(t, []string{"answered", "timeout"}, o.Sent(), "once each, in order; never the unsent")
	assert.Equal(t, []string{"timeout"}, o.Unanswered())
}

func TestPutOutcome_NilIsSafe(t *testing.T) {
	var o *PutOutcome
	o.record("x", errors.New("boom"))
	assert.Nil(t, o.Sent())
}
