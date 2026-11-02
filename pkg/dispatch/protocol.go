package dispatch

import (
	"fmt"
	"net/http"
	"strconv"
)

// ProtocolVersion is the version of the protocol between a controller and an
// agent: the routes under /v1/ and the JSON of Job and Result.
//
// The rules that keep controllers and agents of different ages working
// together, since a machine is made from a snapshot that may be older than the
// controller that calls it:
//
//   - A change that only adds something optional (a field an older reader
//     ignores, a header it does not look at) keeps the version. The reader of an
//     optional field must work without it.
//   - A change that alters what an existing field or route means, or removes
//     one, raises the version. A controller then serves the agents of the
//     versions it still supports (MinProtocolVersion up to its own), and refuses
//     an agent newer than itself, saying so, instead of letting it work with a
//     picture of the protocol the controller does not share.
//   - An agent that does not send the version header (all that were built
//     before it existed) speaks version 1.
const ProtocolVersion = 1

// MinProtocolVersion is the oldest agent a controller of this build serves.
const MinProtocolVersion = 1

// ProtocolHeader carries the version: the agent sends its own with every
// request, the controller its own with every reply.
const ProtocolHeader = "X-Raccoonci-Protocol"

// agentVersion reads the version an agent says it speaks; a request without
// the header is from an agent that predates it, so version 1.
func agentVersion(r *http.Request) (int, error) {
	v := r.Header.Get(ProtocolHeader)
	if v == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("the %s header %q is not a protocol version", ProtocolHeader, v)
	}
	return n, nil
}
