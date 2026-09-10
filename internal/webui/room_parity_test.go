package webui

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRoomParityFixture(t *testing.T) {
	data, err := os.ReadFile("room_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name        string     `json:"name"`
		Content     string     `json:"content"`
		Tags        [][]string `json:"tags"`
		Attachments []string   `json:"attachments"`
		Text        string     `json:"text"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			row := map[string]any{"content": tc.Content, "tags": tc.Tags}
			items := roomAttachments(row)
			if len(items) != len(tc.Attachments) {
				t.Fatalf("attachments: got %d, want %d", len(items), len(tc.Attachments))
			}
			for i, want := range tc.Attachments {
				if items[i].URL != want {
					t.Errorf("attachment %d: got %q, want %q", i, items[i].URL, want)
				}
			}
			rendered := string(renderChatMarkdown(tc.Content))
			if tc.Text != "" && rendered == "" {
				t.Fatal("markdown renderer returned empty output")
			}
		})
	}
}
