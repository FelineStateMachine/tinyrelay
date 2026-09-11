package event

import "encoding/hex"

// RoomReplyRoot reads NIP-10 marked references and the older unmarked
// NIP-29 thread convention. A mention or q tag never makes a reply.
func RoomReplyRoot(e Event) string {
	if e.Kind != KIND_CHAT && e.Kind != KIND_THREAD_REPLY {
		return ""
	}
	var root, reply, first string
	marked := false
	for _, tag := range e.Tags {
		if len(tag) < 2 || tag[0] != "e" || len(tag[1]) != 64 {
			continue
		}
		if _, err := hex.DecodeString(tag[1]); err != nil {
			continue
		}
		marker := ""
		if len(tag) > 3 {
			marker = tag[3]
		}
		if marker != "" {
			marked = true
		}
		switch marker {
		case "root":
			root = tag[1]
		case "reply":
			reply = tag[1]
		case "":
			if first == "" {
				first = tag[1]
			}
		}
	}
	if root != "" {
		return root
	}
	if reply != "" {
		return reply
	}
	if !marked {
		return first
	}
	return ""
}
