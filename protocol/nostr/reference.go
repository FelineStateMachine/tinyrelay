package nostr

// EventRef identifies an event and its creation time without carrying its
// content. Relay synchronization uses these pairs to reconcile event sets.
type EventRef struct {
	ID        string
	Timestamp int64
}
