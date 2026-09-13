package auth

// Session carries the principals authenticated on a relay connection and the
// relay identity against which their proofs were checked. RemoteIP is supplied
// by the serving transport, not by signed event content.
type Session struct {
	PubKeys  []string
	RelayURL string
	RemoteIP string
}
