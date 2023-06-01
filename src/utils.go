package main

import (
	"strings"

	log "github.com/sirupsen/logrus"
)

// contains checks if a given string item is present in a slice of strings arr.
// It returns true if the item is present, false otherwise.
func contains(arr []string, item string) bool {
	for _, v := range arr {
		if strings.EqualFold(v, item) {
			return true
		}
	}
	return false
}

// containsTags checks if any of the tags in the given slice of strings are present in another slice of strings.
// It returns true if any of the tags are present, false otherwise.
// It also logs a message for each tag found in the slice of strings.
func containsTags(arr []string, tags []string) bool {
	found := false
	for _, v := range arr {
		for _, tag := range tags {
			if strings.EqualFold(strings.ToLower(v), strings.ToLower(tag)) {
				found = true
				log.Debugf("stream tag %s found\n", tag)
			}
		}
	}
	return found
}

// lineIndex returns the index of the first line in a slice of strings that contains the given string item.
func lineIndex(arr []string, item string) (i int) {
	i = -1
	for i, v := range arr {
		if strings.Contains(strings.ToLower(v), strings.ToLower(item)) {
			return i
		}
	}
	return
}
