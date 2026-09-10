package event

import "testing"

func TestJobKindClassification(t *testing.T) {
	tests := []struct {
		kind    int
		request bool
		result  bool
		job     bool
		answer  int
	}{
		{kind: 4999},
		{kind: 5000, request: true, job: true, answer: 6000},
		{kind: 5127, request: true, job: true, answer: 6127},
		{kind: KIND_SITE_SNAPSHOT},
		{kind: 5129, request: true, job: true, answer: 6129},
		{kind: 5999, request: true, job: true, answer: 6999},
		{kind: 6000, result: true, job: true},
		{kind: 6128, result: true, job: true},
		{kind: 6999, result: true, job: true},
		{kind: KIND_JOB_FEEDBACK, job: true},
		{kind: 7001},
	}
	for _, tt := range tests {
		if IsJobRequest(tt.kind) != tt.request || IsJobResult(tt.kind) != tt.result || IsJobKind(tt.kind) != tt.job || JobResultKind(tt.kind) != tt.answer {
			t.Errorf("kind %d: request=%v result=%v job=%v answer=%d; want request=%v result=%v job=%v answer=%d", tt.kind, IsJobRequest(tt.kind), IsJobResult(tt.kind), IsJobKind(tt.kind), JobResultKind(tt.kind), tt.request, tt.result, tt.job, tt.answer)
		}
	}
}
