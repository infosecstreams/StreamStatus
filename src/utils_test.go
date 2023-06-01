package main

import (
	"bytes"
	"testing"

	log "github.com/sirupsen/logrus"
)

func TestContains(t *testing.T) {
	arr := []string{"foo", "bar", "baz"}
	item := "bar"
	if !contains(arr, item) {
		t.Errorf("contains(%v, %s) = false, want true", arr, item)
	}

	item = "qux"
	if contains(arr, item) {
		t.Errorf("contains(%v, %s) = true, want false", arr, item)
	}
}

func TestContainsTags(t *testing.T) {
	arr := []string{"foo", "bar", "baz"}
	tags := []string{"qux", "bar", "quux"}
	if !containsTags(arr, tags) {
		t.Errorf("containsTags(%v, %v) = false, want true", arr, tags)
	}

	tags = []string{"qux", "quux"}
	if containsTags(arr, tags) {
		t.Errorf("containsTags(%v, %v) = true, want false", arr, tags)
	}
}

func TestLineIndex(t *testing.T) {
	arr := []string{"foo", "bar", "baz"}
	item := "bar"
	want := 1
	if got := lineIndex(arr, item); got != want {
		t.Errorf("lineIndex(%v, %s) = %d, want %d", arr, item, got, want)
	}

	item = "qux"
	want = -1
	if got := lineIndex(arr, item); got != want {
		t.Errorf("lineIndex(%v, %s) = %d, want %d", arr, item, got, want)
	}
}

func TestContainsTagsLogs(t *testing.T) {
	arr := []string{"foo", "bar", "baz"}
	tags := []string{"qux", "bar", "quux"}

	oldLog := log.StandardLogger().Out
	defer func() { log.StandardLogger().Out = oldLog }()

	logBuf := &bytes.Buffer{}
	log.SetOutput(logBuf)

	if !containsTags(arr, tags) {
		t.Errorf("containsTags(%v, %v) = false, want true", arr, tags)
	}
}
