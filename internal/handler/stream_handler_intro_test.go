package handler

import "testing"

func TestShouldSkipIntroDelta_FirstPass(t *testing.T) {
	h := &streamHandler{introDedup: make(map[string]struct{})}

	if h.shouldSkipIntroDelta("我是 Claude") {
		t.Fatalf("first intro delta should not be skipped")
	}
}

func TestShouldSkipIntroDelta_Duplicate(t *testing.T) {
	h := &streamHandler{introDedup: make(map[string]struct{})}

	if h.shouldSkipIntroDelta("你好") {
		t.Fatalf("first intro delta should not be skipped")
	}
	if !h.shouldSkipIntroDelta("你好") {
		t.Fatalf("duplicate intro delta should be skipped")
	}
}
