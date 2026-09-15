package event

import "testing"

func TestJobKindClassification(t *testing.T) {
	tests := []struct {
		kind                                      int
		request, answer, cancel, terminal, result bool
	}{
		{kind: 5000},
		{kind: KIND_SITE_SNAPSHOT},
		{kind: 7000},
		{kind: 43000},
		{kind: KIND_JOB_REQUEST, request: true},
		{kind: KIND_JOB_ACCEPTED, answer: true},
		{kind: KIND_JOB_PROGRESS, answer: true},
		{kind: KIND_JOB_RESULT, answer: true, terminal: true, result: true},
		{kind: KIND_JOB_CANCEL, cancel: true, terminal: true},
		{kind: KIND_JOB_ERROR, answer: true, terminal: true},
		{kind: 43007},
	}
	for _, tt := range tests {
		job := tt.request || tt.answer || tt.cancel
		if IsJobRequest(tt.kind) != tt.request || IsJobAnswer(tt.kind) != tt.answer || IsJobCancel(tt.kind) != tt.cancel || IsJobTerminal(tt.kind) != tt.terminal || IsJobResult(tt.kind) != tt.result || IsJobKind(tt.kind) != job {
			t.Errorf("kind %d: request=%v answer=%v cancel=%v terminal=%v result=%v job=%v", tt.kind, IsJobRequest(tt.kind), IsJobAnswer(tt.kind), IsJobCancel(tt.kind), IsJobTerminal(tt.kind), IsJobResult(tt.kind), IsJobKind(tt.kind))
		}
	}
}
