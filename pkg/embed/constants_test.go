package embed

import (
	"testing"

	contract "github.com/inference-sim/blis-substrate/contracts/embed"

	sessionidconstants "github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/requestcontrol/dataproducer/sessionid/constants"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/plugins/scheduling/util/sessionaffinity"
)

// TestSessionConstantsAgreeWithTheirOwners: the header the host sends a session id under (the
// contract's), the one llm-d's plugins read by default, and the token header the bridge reports are
// each the owning constant, so a change in either owner moves the bridge with it or fails here.
func TestSessionConstantsAgreeWithTheirOwners(t *testing.T) {
	if contract.SessionIDHeader != sessionaffinity.DefaultSessionIDHeader {
		t.Errorf("the contract sends %q, the plugins read %q", contract.SessionIDHeader, sessionaffinity.DefaultSessionIDHeader)
	}
	h := requestHeaderMap(contract.Request{SessionID: "s1"})
	if h[sessionaffinity.DefaultSessionIDHeader] != "s1" {
		t.Errorf("a request's SessionID is not sent under the plugins' header %q: %v", sessionaffinity.DefaultSessionIDHeader, h)
	}
	ids := sessionIdentityFacts("session-affinity-scorer", "s", map[string]any{"strategy": contract.SessionStrategyID})
	if len(ids) != 1 || ids[0].Header != sessionaffinity.DefaultSessionIDHeader {
		t.Errorf("a session_id plugin with no sources reports %+v, want header %q", ids, sessionaffinity.DefaultSessionIDHeader)
	}
	ids = sessionIdentityFacts("session-affinity-scorer", "s", nil)
	if len(ids) != 1 || ids[0].Header != sessionaffinity.DefaultHeader {
		t.Errorf("an encoded-endpoint plugin reports %+v, want header %q", ids, sessionaffinity.DefaultHeader)
	}
	if !audited[sessionidconstants.SessionIDProducerType] {
		t.Errorf("%s is not audited", sessionidconstants.SessionIDProducerType)
	}
}
