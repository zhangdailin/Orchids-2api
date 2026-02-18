package grok

import "testing"

func TestLooksLikeImageRequest(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   bool
	}{
		{name: "empty", prompt: "", want: false},
		{name: "chinese_positive", prompt: "帮我生成一张图片", want: true},
		{name: "english_positive", prompt: "show me an image of a cat", want: true},
		{name: "negative_override", prompt: "上一轮提到图片，这轮不要图片，只文字回答", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := looksLikeImageRequest(tt.prompt)
			if got != tt.want {
				t.Fatalf("looksLikeImageRequest(%q)=%v want=%v", tt.prompt, got, tt.want)
			}
		})
	}
}
