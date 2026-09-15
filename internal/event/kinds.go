package event

const (
	KIND_PROFILE             = 0
	KIND_DELETION            = 5
	KIND_VANISH              = 62
	KIND_AUTH                = 22242
	KIND_DM                  = 14
	KIND_WRAP                = 1059
	KIND_REPORT              = 1984
	KIND_NOSTR_CONNECT       = 24133
	KIND_APP_DATA            = 30078
	KIND_VIEW                = KIND_APP_DATA
	KIND_PRESENCE            = 20078
	KIND_MEMBER_ADDED        = 8000
	KIND_MEMBER_REMOVED      = 8001
	KIND_ROSTER              = 13534
	KIND_ROLE_DEF            = 33534
	KIND_NIP43_JOIN          = 28934
	KIND_NIP43_INVITE        = 28935
	KIND_NIP43_LEAVE         = 28936
	KIND_PUT_USER            = 9000
	KIND_REMOVE_USER         = 9001
	KIND_EDIT_METADATA       = 9002
	KIND_DELETE_EVENT        = 9005
	KIND_CREATE_GROUP        = 9007
	KIND_DELETE_GROUP        = 9008
	KIND_CREATE_INVITE       = 9009
	KIND_PINS                = 9010
	KIND_JOIN                = 9021
	KIND_LEAVE               = 9022
	KIND_GROUP_METADATA      = 39000
	KIND_GROUP_ADMINS        = 39001
	KIND_GROUP_MEMBERS       = 39002
	KIND_GROUP_ROLES         = 39003
	KIND_GROUP_PINS          = 39005
	KIND_RELAY_DISCOVERY     = 30166
	KIND_SITE                = 15128
	KIND_NAMED_SITE          = 35128
	KIND_SITE_SNAPSHOT       = 5128
	KIND_MARMOT_GROUP        = 445
	KIND_MARMOT_KEY_PACKAGE  = 30443
	KIND_REPO                = 30617
	KIND_REPO_STATE          = 30618
	KIND_GIT_PATCH           = 1617
	KIND_GIT_PR              = 1618
	KIND_GIT_PR_UPDATE       = 1619
	KIND_GIT_ISSUE           = 1621
	KIND_PUSH_REGISTRATION   = 30390
	KIND_CHAT                = 9
	KIND_THREAD              = 11
	KIND_THREAD_REPLY        = 12
	KIND_ROOM_PRESENCE       = 20001
	KIND_ROOM_TYPING         = 20002
	KIND_RICH_CONTENT        = 40002
	KIND_CONTENT_EDIT        = 40003
	KIND_ROOM_MEMBER_ADDED   = 44100
	KIND_ROOM_MEMBER_REMOVED = 44101

	// NIP-54 wiki: articles, merge requests and redirects.
	KIND_WIKI_ARTICLE  = 30818
	KIND_WIKI_MERGE    = 818
	KIND_WIKI_REDIRECT = 30819
	KIND_AGENT_GRANT   = 30392
)

// Long tasks use the six job kinds Buzz reserves. A requester publishes a
// request; the keys it asks answer while they work and finish with a result
// or an error; the requester may cancel. Kind 5128 belongs to NIP-5A site
// snapshots and is unrelated.
const (
	KIND_JOB_REQUEST  = 43001
	KIND_JOB_ACCEPTED = 43002
	KIND_JOB_PROGRESS = 43003
	KIND_JOB_RESULT   = 43004
	KIND_JOB_CANCEL   = 43005
	KIND_JOB_ERROR    = 43006
)

// IsJobRequest reports whether kind is a long-task request.
func IsJobRequest(kind int) bool { return kind == KIND_JOB_REQUEST }

// IsJobResult reports whether kind is a long-task result.
func IsJobResult(kind int) bool { return kind == KIND_JOB_RESULT }

// IsJobAnswer reports whether kind is one of the answers an asked key
// publishes: accepted, progress, result or error.
func IsJobAnswer(kind int) bool {
	return kind == KIND_JOB_ACCEPTED || kind == KIND_JOB_PROGRESS || kind == KIND_JOB_RESULT || kind == KIND_JOB_ERROR
}

// IsJobCancel reports whether kind is a requester's cancel.
func IsJobCancel(kind int) bool { return kind == KIND_JOB_CANCEL }

// IsJobTerminal reports whether kind ends a long task: result, cancel or
// error.
func IsJobTerminal(kind int) bool {
	return kind == KIND_JOB_RESULT || kind == KIND_JOB_CANCEL || kind == KIND_JOB_ERROR
}

// IsJobKind reports whether kind is any of the six long-task kinds.
func IsJobKind(kind int) bool {
	return IsJobRequest(kind) || IsJobAnswer(kind) || IsJobCancel(kind)
}
