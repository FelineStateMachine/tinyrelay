package daemon

import (
	"errors"
	"strconv"
	"strings"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
)

// Review comments. A kind 1111 comment under a pull request or patch may name
// one line of the diff with ["file", "<path>"] and ["line", "<n>", "<side>"],
// where the side is "old" for the base or "new" for the proposed version. The
// relay validates the shape whenever the tags are present and browse results
// carry the anchor as plain fields so clients can place the comment.

const reviewFileMax = 4096

// reviewAnchor is the diff line a comment names. A zero value means the
// comment is not anchored.
type reviewAnchor struct {
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	Side string `json:"side,omitempty"`
}

// collaborationReply is one comment in a thread with its anchor, when it has
// one, and its proposal state alongside the event fields.
type collaborationReply struct {
	event.Event
	reviewAnchor
	collaborationProposal
}

// collaborationReplies pairs each reply with its anchor and the proposal
// state at the same index of states.
func collaborationReplies(rows []event.Event, states []collaborationProposal) []collaborationReply {
	out := make([]collaborationReply, 0, len(rows))
	for i, row := range rows {
		anchor, _, _ := reviewAnchorOf(row)
		reply := collaborationReply{Event: row, reviewAnchor: anchor}
		if i < len(states) {
			reply.collaborationProposal = states[i]
		}
		out = append(out, reply)
	}
	return out
}

// reviewAnchorOf reads a comment's anchor. It reports whether the tags are
// present and returns a blocked reason when they are malformed.
func reviewAnchorOf(e event.Event) (reviewAnchor, bool, error) {
	if e.Kind != 1111 {
		return reviewAnchor{}, false, nil
	}
	var files, lines [][]string
	for _, tag := range e.Tags {
		if len(tag) == 0 {
			continue
		}
		switch tag[0] {
		case "file":
			files = append(files, tag)
		case "line":
			lines = append(lines, tag)
		}
	}
	if len(files) == 0 && len(lines) == 0 {
		return reviewAnchor{}, false, nil
	}
	if len(files) != 1 || len(lines) != 1 {
		return reviewAnchor{}, true, errors.New("blocked: a review comment carries one file tag and one line tag")
	}
	file := files[0]
	if len(file) < 2 || strings.TrimSpace(file[1]) == "" || len(file[1]) > reviewFileMax || strings.ContainsAny(file[1], "\n\r\x00") {
		return reviewAnchor{}, true, errors.New("blocked: a review comment file tag needs a path")
	}
	line := lines[0]
	if len(line) < 3 {
		return reviewAnchor{}, true, errors.New("blocked: a review comment line tag needs a line number and a side, old or new")
	}
	n, err := strconv.Atoi(line[1])
	if err != nil || n < 1 || strconv.Itoa(n) != line[1] {
		return reviewAnchor{}, true, errors.New("blocked: a review comment line must be a positive integer")
	}
	if line[2] != "old" && line[2] != "new" {
		return reviewAnchor{}, true, errors.New("blocked: a review comment side must be old or new")
	}
	if kind := event.Tag(e, "K"); kind != "1617" && kind != "1618" {
		return reviewAnchor{}, true, errors.New("blocked: a review comment anchors to a pull request or patch")
	}
	return reviewAnchor{File: file[1], Line: n, Side: line[2]}, true, nil
}

// validateReviewAnchor is the admission check for comments that carry file
// and line tags. Comments without them pass.
func validateReviewAnchor(e event.Event) error {
	_, _, err := reviewAnchorOf(e)
	return err
}
