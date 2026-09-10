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

// NIP-90 long tasks use request kinds 5000 to 5127 and 5129 to 5999.
// Each request is answered by a result kind 1000 higher and by feedback
// of kind 7000. Kind 5128 belongs to NIP-5A site snapshots.
const (
	KIND_JOB_REQUEST_MIN = 5000
	KIND_JOB_REQUEST_MAX = 5999
	KIND_JOB_RESULT_MIN  = 6000
	KIND_JOB_RESULT_MAX  = 6999
	KIND_JOB_FEEDBACK    = 7000
)

// JobFeedbackStatuses is the NIP-90 vocabulary of the feedback status tag.
var JobFeedbackStatuses = []string{"payment-required", "processing", "error", "success", "partial"}

// JobInputTypes is the NIP-90 vocabulary of the i tag's input type.
var JobInputTypes = []string{"url", "event", "job", "text"}

// IsJobRequest reports whether kind belongs to long-task requests.
// NIP-5A site snapshots have their own admission and manifest rules.
func IsJobRequest(kind int) bool {
	return kind >= KIND_JOB_REQUEST_MIN && kind <= KIND_JOB_REQUEST_MAX && kind != KIND_SITE_SNAPSHOT
}

// IsJobResult reports whether kind is a NIP-90 result kind.
func IsJobResult(kind int) bool { return kind >= KIND_JOB_RESULT_MIN && kind <= KIND_JOB_RESULT_MAX }

// IsJobKind reports whether the kind is a job request, result or feedback.
func IsJobKind(kind int) bool {
	return IsJobRequest(kind) || IsJobResult(kind) || kind == KIND_JOB_FEEDBACK
}

// JobResultKind returns the result kind for a request kind, or 0 for other
// kinds.
func JobResultKind(request int) int {
	if !IsJobRequest(request) {
		return 0
	}
	return request + 1000
}

// IsJobFeedbackStatus reports whether status is in the NIP-90 feedback
// vocabulary.
func IsJobFeedbackStatus(status string) bool {
	for _, known := range JobFeedbackStatuses {
		if known == status {
			return true
		}
	}
	return false
}

// IsJobInputType reports whether kind is a supported NIP-90 input type.
func IsJobInputType(kind string) bool {
	for _, known := range JobInputTypes {
		if known == kind {
			return true
		}
	}
	return false
}
